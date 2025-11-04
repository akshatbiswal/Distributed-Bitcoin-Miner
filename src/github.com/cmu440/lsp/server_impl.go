// Contains the implementation of a LSP server.
package lsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cmu440/lspnet"
)

// Server definition, manages all endpoints over UDP socket,
// manages traffic, epochs, input output.
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

// Sets up all the state to start a new server and starts the new server.
// This function is application facintg
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

	// main 3 goroutines
	go s.readLoop()
	go s.mainLoop()
	go s.startEpochTimer()

	return s, nil
}

// This is application facing, and returns in roder messages from clients
func (s *server) Read() (int, []byte, error) {
	select {
	// appReadQ is the final destination for in order messages
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

// This function is application facing, and sends the payload and connid
// to the main loop through the appWriteQ channell
func (s *server) Write(connId int, payload []byte) error {
	p := append([]byte(nil), payload...)
	// appWriteQ is the entry point for any message to write
	s.appWriteQ <- serverWrite{connID: connId, payload: p}
	return nil
}

// Closses a specif connection through a channell
func (s *server) CloseConn(connId int) error {
	go func() { s.closeConnReq <- connId }()
	return nil
}

// Closses the entinte server, which is started by a channell
func (s *server) Close() error {
	s.closeAllReq <- struct{}{}
	return <-s.closed
}

// Reads all data from clients, and puts them in a channell that
// the main loop will read from
func (s *server) readLoop() {
	// Max size for a UPD packet
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
		// Put the message into a netEven which represent an incoming message
		// netinCh is the initial entry point for any messsages from clients
		s.netInCh <- netEvent{msg: m, addr: addr}
	}
}

// For the most part all state changes happen in the main loop
// other loops will send information into this main loop to get actions
// completed
func (s *server) mainLoop() {
	for {
		// This loop sends any messages to each client if possible
		for _, ep := range s.conns {
			for s.canSendMore(ep) && len(ep.sendQ) > 0 {
				seq := ep.nextSendSeq
				p := ep.sendQ[0]
				if err := s.sendData(ep, seq, p); err != nil {
					break
				}
				// Remove the first element from the sending queue for that client
				ep.sendQ = ep.sendQ[1:]
				ep.nextSendSeq++
			}
		}

		var outCh chan serverMsg
		var outNext serverMsg
		// Try to put something from readyDeliveri nto the final output queue
		// appReadQ
		if len(s.readyDeliver) > 0 {
			outCh = s.appReadQ
			outNext = s.readyDeliver[0]
		}
		// Can only close when everything that needs to be sent to clients is sent
		if s.closingAll && s.allDrained() {
			close(s.stopEpochCh)
			_ = s.conn.Close()
			s.closed <- nil
			return
		}

		select {
		// netInCh is the entry point for the read loop, so we need to process the message
		case ev := <-s.netInCh:
			s.processIncoming(ev.msg, ev.addr)
		// appWriteQ is the entry point for application writes, so we need to try to send
		// it to the desired client, so append it to the sendQ for that client
		case w := <-s.appWriteQ:
			if ep, ok := s.conns[w.connID]; ok && !ep.closing {
				ep.sendQ = append(ep.sendQ, w.payload)
			}

		case id := <-s.closeConnReq:
			// Closing one client means that we need to change the readyDeliver and remove
			// any messages to be sent to the clossing conn id needs to be removed
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
		// True to add first elemetn of readyDeliver to appWriteQ
		case outCh <- outNext:
			s.readyDeliver = s.readyDeliver[1:]

		case <-s.epochTickCh:
			s.handleEpoch()

		}
	}
}

// Thi function is a helper function for the main loop for when
// there is an epoch tick
func (s *server) handleEpoch() {
	for id, ep := range s.conns {
		resend := false
		// update backoff state for each message
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
			// otherwise we need to send this epoch
			// and do exopenetial backoff
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
		// If we havn't send anything and the client isn't closed we need to send a heartbeat
		if !ep.sentSinceLastEpoch && !resend && !ep.closing {
			_ = s.sendTo(ep.addr, NewAck(id, 0))
		}
		ep.epochsSinceRecv++
		if ep.epochsSinceRecv >= s.params.EpochLimit {
			// delete state for client
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
			// If we don't close connection, reset the sent check
			ep.sentSinceLastEpoch = false
		}
	}
}

// This function is called by the main loop take a message and
// perform the necessary operations.
func (s *server) processIncoming(m Message, addr *lspnet.UDPAddr) {
	switch m.Type {
	case MsgConnect:
		key := addr.String()
		id, ok := s.byAddr[key]
		// This is a new connect message, has not been recieved yet
		if !ok {
			// Increment connID
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
			// Put the state of client in conns map
			s.conns[id] = ep
			// Map the adress to connID for easy acess
			s.byAddr[key] = id
		} else {
			if ep, ok := s.conns[id]; ok {
				ep.epochsSinceRecv = 0
			}
		}
		//Always send ack to client for handshake
		_ = s.sendTo(addr, NewAck(id, m.SeqNum))

	case MsgAck:
		// We got an ack from a client, so reset ephochs
		// We can remove the state that represents that the sequence
		// number is in transit
		if ep, ok := s.conns[m.ConnID]; ok {
			ep.epochsSinceRecv = 0
			delete(ep.inflight, m.SeqNum)
			delete(ep.backoff, m.SeqNum)
		}

	case MsgCAck:
		// Same idea as Ack, just loop through all sequence numbers
		// in the in transit messages
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
		// Always need to reset epoch
		ep.epochsSinceRecv = 0
		if m.Size < 0 {
			return
		}
		// This is specific to this implementation, in terms of we consider shorter messages
		// corrupt and longer messages to be truncated
		if len(m.Payload) < m.Size {
			return
		}
		if len(m.Payload) > m.Size {
			m.Payload = m.Payload[:m.Size]
		}
		if CalculateChecksum(m.ConnID, m.SeqNum, m.Size, m.Payload) != m.Checksum {
			return
		}
		// Always need to send an acknowledgement
		_ = s.sendTo(ep.addr, NewAck(m.ConnID, m.SeqNum))

		if ep.closing {
			return
		}
		// We already recived this SeqNum, the client just hasn't recieved our ack
		// but we just sent it above so we can return
		if m.SeqNum < ep.nextRecvSeq {
			return
		}
		if m.SeqNum == ep.nextRecvSeq {
			s.readyDeliver = append(s.readyDeliver, serverMsg{connID: m.ConnID, payload: m.Payload})
			ep.nextRecvSeq++
			// The recvBuf has all sequence number that are too high becuase we havn't revied all
			// seq number less that it, so we add all the seq number after the seqNum we just got,
			// and stop when there is a new seq number that we havn't revieced yet.
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
		// SeqNum is too high, havn't revieved all seq number less that the one we just got
		ep.recvBuf[m.SeqNum] = m.Payload
	}
}

// Helper function that looks through inflight messages of client
// and returns the lowest seq number and whether it is the first or not
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

// Helper function that looks at whehher another message can be inflight
// by looking at the maximum unacked messages and window size(implements)
// the check for the sliding window algorithm
func (s *server) canSendMore(ep *endpoint) bool {
	// MacUnackedMessages < WindowSize, so we check it first
	if len(ep.inflight) >= s.params.MaxUnackedMessages {
		return false
	}
	// Then chekc whether we actually have the window size
	if base, ok := s.oldestUnacked(ep); ok {
		if ep.nextSendSeq-base >= s.params.WindowSize {
			return false
		}
	}
	return true
}

// Sends data to some endpoint and labels that data with some sequence number
// It also adds the message the necessary maps
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

// This function adds marshaling to writing a specific message to some
// address
func (s *server) sendTo(addr *lspnet.UDPAddr, m *Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.conn.WriteToUDP(b, addr)
	return err
}

// Heapler function to access byAddr without dealing with null.
func (s *server) connIDFor(addr *lspnet.UDPAddr) int {
	if addr == nil {
		return 0
	}
	if id, ok := s.byAddr[addr.String()]; ok {
		return id
	}
	return 0
}

// Checks if all messages that need to be sent have been sent
func (s *server) allDrained() bool {
	for _, ep := range s.conns {
		if len(ep.sendQ) > 0 || len(ep.inflight) > 0 {
			return false
		}
	}
	return true
}

// Starts the ticker, the main logic is in the main loop
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
		// stops go routine when close occurs
		case <-s.stopEpochCh:
			return
		}
	}
}
