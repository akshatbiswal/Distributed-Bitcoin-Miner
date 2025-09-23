// Contains the implementation of a LSP server.
package lsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cmu440/lspnet"
)

type server struct {
	// fixed
	conn   *lspnet.UDPConn
	params *Params

	// connection table
	nextConnID int
	conns      map[int]*endpoint
	byAddr     map[string]int

	// app-facing delivery
	appReadQ chan serverMsg
	appErrQ  chan serverErr

	// control
	appWriteQ    chan serverWrite
	closeConnReq chan int
	closeAllReq  chan struct{}
	closed       chan error

	// internal events
	netInCh      chan netEvent
	readyDeliver []serverMsg

	// state
	closingAll bool
}

type serverMsg struct {
	connID  int
	payload []byte
}

type serverErr struct {
	connID int
	err    error
}

type serverWrite struct {
	connID  int
	payload []byte
}

type netEvent struct {
	msg  Message
	addr *lspnet.UDPAddr
}

type endpoint struct {
	addr *lspnet.UDPAddr

	// send-side (server to that specific client)
	nextSendSeq int
	inflight    map[int]*Message
	sendQ       [][]byte

	// recv-side (specificclient to server)
	nextRecvSeq int
	recvBuf     map[int][]byte

	// close state
	closing bool
}

func NewServer(port int, params *Params) (Server, error) {
	if params == nil {
		params = NewParams()
	}
	laddr, err := lspnet.ResolveUDPAddr("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	conn, err := lspnet.ListenUDP("udp", laddr)
	if err != nil {
		return nil, err
	}
	s := &server{
		conn:         conn,
		params:       params,
		nextConnID:   1,
		conns:        make(map[int]*endpoint),
		byAddr:       make(map[string]int),
		appReadQ:     make(chan serverMsg),
		appErrQ:      make(chan serverErr),
		appWriteQ:    make(chan serverWrite),
		closeConnReq: make(chan int),
		closeAllReq:  make(chan struct{}),
		closed:       make(chan error, 1),
		netInCh:      make(chan netEvent, 1),
		readyDeliver: make([]serverMsg, 0),
	}

	go s.readLoop()
	go s.mainLoop()

	return s, nil
}

func (s *server) Read() (int, []byte, error) {
	select {
	case m := <-s.appReadQ:
		return m.connID, m.payload, nil
	case e := <-s.appErrQ:
		return e.connID, nil, e.err
	case err := <-s.closed:
		if err == nil {
			err = errors.New("server closed")
		}
		return 0, nil, err
	}
}

func (s *server) Write(connId int, payload []byte) error {
	p := append([]byte(nil), payload...)
	// go func() {
	// 	s.appWriteQ <- serverWrite{connID: connId, payload: p}
	// }()
	s.appWriteQ <- serverWrite{connID: connId, payload: p}
	return nil
}

func (s *server) CloseConn(connId int) error {
	go func() { s.closeConnReq <- connId }()
	return nil
}

func (s *server) Close() error {
	s.closeAllReq <- struct{}{}
	return <-s.closed
}

func (s *server) readLoop() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var m Message
		if err := json.Unmarshal(buf[:n], &m); err != nil {
			continue
		}
		s.netInCh <- netEvent{msg: m, addr: addr}
	}
}

func (s *server) mainLoop() {
	for {
		for _, ep := range s.conns {
			for s.canSendMore(ep) && len(ep.sendQ) > 0 {
				seq := ep.nextSendSeq
				p := ep.sendQ[0]
				if err := s.sendData(ep, seq, p); err != nil {
					break
				}
				ep.sendQ = ep.sendQ[1:]
				ep.nextSendSeq++
			}
		}

		var outCh chan serverMsg
		var outNext serverMsg
		if len(s.readyDeliver) > 0 {
			outCh = s.appReadQ
			outNext = s.readyDeliver[0]
		}

		if s.closingAll && s.allDrained() {
			_ = s.conn.Close()
			s.closed <- nil
			return
		}

		select {
		case ev := <-s.netInCh:
			s.processIncoming(ev.msg, ev.addr)

		case w := <-s.appWriteQ:
			if ep, ok := s.conns[w.connID]; ok && !ep.closing {
				ep.sendQ = append(ep.sendQ, w.payload)
			}

		case id := <-s.closeConnReq:
			if ep, ok := s.conns[id]; ok {
				ep.closing = true
				dst := s.readyDeliver[:0]
				for _, it := range s.readyDeliver {
					if it.connID != id {
						dst = append(dst, it)
					}
				}
				s.readyDeliver = dst
				go func() { s.appErrQ <- serverErr{connID: id, err: errors.New("connection closed")} }()
			}

		case <-s.closeAllReq:
			s.closingAll = true
			for _, ep := range s.conns {
				ep.closing = true
			}
			s.readyDeliver = s.readyDeliver[:0]

		case outCh <- outNext:
			s.readyDeliver = s.readyDeliver[1:]
		}
	}
}

func (s *server) processIncoming(m Message, addr *lspnet.UDPAddr) {
	switch m.Type {
	case MsgConnect:
		key := addr.String()
		id, ok := s.byAddr[key]
		if !ok {
			id = s.nextConnID
			s.nextConnID++
			ep := &endpoint{
				addr:        addr,
				nextSendSeq: m.SeqNum + 1,
				inflight:    make(map[int]*Message),
				sendQ:       make([][]byte, 0),
				nextRecvSeq: m.SeqNum + 1,
				recvBuf:     make(map[int][]byte),
			}
			s.conns[id] = ep
			s.byAddr[key] = id
		}
		_ = s.sendTo(addr, NewAck(id, m.SeqNum))

	case MsgAck:
		if ep, ok := s.conns[m.ConnID]; ok {
			delete(ep.inflight, m.SeqNum)
		}

	case MsgCAck:
		if ep, ok := s.conns[m.ConnID]; ok {
			for seq := range ep.inflight {
				if seq <= m.SeqNum {
					delete(ep.inflight, seq)
				}
			}
		}

	case MsgData:
		ep, ok := s.conns[m.ConnID]
		if !ok {
			return
		}
		if m.Size < 0 || m.Size > len(m.Payload) {
			return
		}
		p := m.Payload[:m.Size]
		_ = s.sendTo(ep.addr, NewAck(m.ConnID, m.SeqNum))

		if ep.closing {
			return
		}

		if m.SeqNum < ep.nextRecvSeq {
			return
		}
		if m.SeqNum == ep.nextRecvSeq {
			s.readyDeliver = append(s.readyDeliver, serverMsg{connID: m.ConnID, payload: p})
			ep.nextRecvSeq++
			for {
				if bufp, ok := ep.recvBuf[ep.nextRecvSeq]; ok {
					delete(ep.recvBuf, ep.nextRecvSeq)
					s.readyDeliver = append(s.readyDeliver, serverMsg{connID: m.ConnID, payload: bufp})
					ep.nextRecvSeq++
					continue
				}
				break
			}
			return
		}
		ep.recvBuf[m.SeqNum] = p
	}
}

func (s *server) canSendMore(ep *endpoint) bool {
	inflight := len(ep.inflight)
	limit := s.params.WindowSize
	if s.params.MaxUnackedMessages < limit {
		limit = s.params.MaxUnackedMessages
	}
	return inflight < limit
}

func (s *server) sendData(ep *endpoint, seq int, p []byte) error {
	id := s.connIDFor(ep.addr)
	cs := CalculateChecksum(id, seq, len(p), p)
	msg := NewData(id, seq, len(p), p, cs)
	if err := s.sendTo(ep.addr, msg); err != nil {
		return err
	}
	ep.inflight[seq] = msg
	return nil
}

func (s *server) sendTo(addr *lspnet.UDPAddr, m *Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.conn.WriteToUDP(b, addr)
	return err
}

func (s *server) connIDFor(addr *lspnet.UDPAddr) int {
	if addr == nil {
		return 0
	}
	if id, ok := s.byAddr[addr.String()]; ok {
		return id
	}
	return 0
}

func (s *server) allDrained() bool {
	for _, ep := range s.conns {
		if len(ep.sendQ) > 0 || len(ep.inflight) > 0 {
			return false
		}
	}
	return true
}
