// Contains the implementation of a LSP server.
package lsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cmu440/lspnet"
	"time"
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

	// epochs
	epochTickCh chan struct{}
	stopEpochCh chan struct{}
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

	// epochs
	epochsSinceRecv    int
	sentSinceLastEpoch bool
	backoff            map[int]*backoffState
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
		epochTickCh:  make(chan struct{}, 1),
		stopEpochCh:  make(chan struct{}),
	}

	go s.readLoop()
	go s.mainLoop()
	go s.startEpochTimer()

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
	udpBufSize := 65507
	buf := make([]byte, udpBufSize)
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
			close(s.stopEpochCh)
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

		case <-s.epochTickCh:
			s.handleEpoch()

		}
	}
}

func (s *server) handleEpoch() {
	for id, ep := range s.conns {
		resend := false
		for seq, msg := range ep.inflight {
			st := ep.backoff[seq]
			if st == nil {
				st = &backoffState{}
				ep.backoff[seq] = st
			}
			if st.remaining > 0 {
				st.remaining--
				continue
			}
			_ = s.sendTo(ep.addr, msg)
			resend = true
			ep.sentSinceLastEpoch = true
			if st.currentBackoff == 0 {
				st.currentBackoff = 1
			} else if s.params.MaxBackOffInterval > 0 {
				st.currentBackoff *= 2
				if st.currentBackoff > s.params.MaxBackOffInterval {
					st.currentBackoff = s.params.MaxBackOffInterval
				}
			}
			st.remaining = st.currentBackoff
		}
		if !ep.sentSinceLastEpoch && !resend && !ep.closing {
			_ = s.sendTo(ep.addr, NewAck(id, 0))
		}
		ep.epochsSinceRecv++
		if ep.epochsSinceRecv >= s.params.EpochLimit {
			delete(s.conns, id)
			for k, v := range s.byAddr {
				if v == id {
					delete(s.byAddr, k)
					break
				}
			}
			dst := s.readyDeliver[:0]
			for _, it := range s.readyDeliver {
				if it.connID != id {
					dst = append(dst, it)
				}
			}
			s.readyDeliver = dst
			for _, it := range s.readyDeliver {
				if it.connID != id {
					dst = append(dst, it)
				}
			}
			s.readyDeliver = dst
			go func() { s.appErrQ <- serverErr{connID: id, err: errors.New("epoch timeout")} }()
		} else {
			ep.sentSinceLastEpoch = false
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
				backoff:     make(map[int]*backoffState),
			}
			s.conns[id] = ep
			s.byAddr[key] = id
		} else {
			if ep, ok := s.conns[id]; ok {
				ep.epochsSinceRecv = 0
			}
		}
		_ = s.sendTo(addr, NewAck(id, m.SeqNum))

	case MsgAck:
		if ep, ok := s.conns[m.ConnID]; ok {
			ep.epochsSinceRecv = 0
			delete(ep.inflight, m.SeqNum)
			delete(ep.backoff, m.SeqNum)
		}

	case MsgCAck:
		if ep, ok := s.conns[m.ConnID]; ok {
			ep.epochsSinceRecv = 0
			for seq := range ep.inflight {
				if seq <= m.SeqNum {
					delete(ep.inflight, seq)
					delete(ep.backoff, seq)
				}
			}
		}

	case MsgData:
		ep, ok := s.conns[m.ConnID]
		if !ok {
			return
		}
		ep.epochsSinceRecv = 0
		if m.Size < 0 {
			return
		}
		if len(m.Payload) < m.Size {
			return
		}
		if len(m.Payload) > m.Size {
			m.Payload = m.Payload[:m.Size]
		}
		if CalculateChecksum(m.ConnID, m.SeqNum, m.Size, m.Payload) != m.Checksum {
			return
		}
		//p := m.Payload[:m.Size]
		_ = s.sendTo(ep.addr, NewAck(m.ConnID, m.SeqNum))

		if ep.closing {
			return
		}

		if m.SeqNum < ep.nextRecvSeq {
			return
		}
		if m.SeqNum == ep.nextRecvSeq {
			s.readyDeliver = append(s.readyDeliver, serverMsg{connID: m.ConnID, payload: m.Payload})
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
		ep.recvBuf[m.SeqNum] = m.Payload
	}
}

func (s *server) oldestUnacked(ep *endpoint) (int, bool) {
	min := 0
	first := true
	for seq := range ep.inflight {
		if first || seq < min {
			min = seq
			first = false
		}
	}
	return min, !first
}
func (s *server) canSendMore(ep *endpoint) bool {
	if len(ep.inflight) >= s.params.MaxUnackedMessages {
		return false
	}
	if base, ok := s.oldestUnacked(ep); ok {
		if ep.nextSendSeq-base >= s.params.WindowSize {
			return false
		}
	}
	return true
}

func (s *server) sendData(ep *endpoint, seq int, p []byte) error {
	id := s.connIDFor(ep.addr)
	cs := CalculateChecksum(id, seq, len(p), p)
	msg := NewData(id, seq, len(p), p, cs)
	if err := s.sendTo(ep.addr, msg); err != nil {
		return err
	}
	ep.inflight[seq] = msg
	if _, ok := ep.backoff[seq]; !ok {
		ep.backoff[seq] = &backoffState{currentBackoff: 0, remaining: 0}
	}
	ep.sentSinceLastEpoch = true
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

func (s *server) startEpochTimer() {
	t := time.NewTicker(time.Duration(s.params.EpochMillis) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			select {
			case s.epochTickCh <- struct{}{}:
			default:
			}
		case <-s.stopEpochCh:
			return
		}
	}
}
