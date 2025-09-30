// Contains the implementation of a LSP client.

package lsp

import (
	"encoding/json"
	"errors"
	"github.com/cmu440/lspnet"
	"time"
)

type client struct {
	// connection, params
	conn       *lspnet.UDPConn
	serverAddr *lspnet.UDPAddr
	params     *Params

	// ids & seq nums
	connID      int
	isn         int
	nextSendSeq int
	nextRecvSeq int

	// send-side state (sliding window)
	windowSize int
	maxUnacked int
	inflight   map[int]*Message
	sendQ      [][]byte

	// recv-side state
	recvBuf   map[int][]byte
	deliverQ  [][]byte
	appReadCh chan []byte

	// app -> client
	appWriteCh chan []byte
	closeReq   chan struct{}
	closed     chan error

	// internal events
	netInCh     chan Message
	connAckCh   chan struct{}
	stopped     chan struct{}
	readStopped chan struct{}

	// epochs
	epochTickCh        chan struct{}
	stopEpochCh        chan struct{}
	epochsSinceRecv    int
	sentSinceLastEpoch bool
	connectEpochs      int
	backoff            map[int]*backoffState
	connErrCh          chan error

	serverStart bool
}

type backoffState struct {
	currentBackoff int
	remaining      int
}

// NewClient creates, initiates, and returns a new client. This function
// should return after a connection with the server has been established
// (i.e., the client has received an Ack message from the server in response
// to its connection request), and should return a non-nil error if a
// connection could not be made (i.e., if after K epochs, the client still
// hasn't received an Ack message from the server in response to its K
// connection requests).
//
// initialSeqNum is an int representing the Initial Sequence Number (ISN) this
// client must use. You may assume that sequence numbers do not wrap around.
//
// hostport is a colon-separated string identifying the server's host address
// and port number (i.e., "localhost:9999").
func NewClient(hostport string, initialSeqNum int, params *Params) (Client, error) {
	raddr, err := lspnet.ResolveUDPAddr("udp", hostport)
	if err != nil {
		return nil, err
	}
	conn, err := lspnet.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = NewParams()
	}
	c := &client{
		conn:               conn,
		serverAddr:         raddr,
		params:             params,
		isn:                initialSeqNum,
		nextSendSeq:        initialSeqNum + 1,
		windowSize:         params.WindowSize,
		maxUnacked:         params.MaxUnackedMessages,
		inflight:           make(map[int]*Message),
		sendQ:              make([][]byte, 0),
		recvBuf:            make(map[int][]byte),
		deliverQ:           make([][]byte, 0),
		appReadCh:          make(chan []byte),
		appWriteCh:         make(chan []byte),
		closeReq:           make(chan struct{}),
		closed:             make(chan error, 1),
		netInCh:            make(chan Message, 1),
		connAckCh:          make(chan struct{}, 1),
		stopped:            make(chan struct{}),
		readStopped:        make(chan struct{}),
		epochTickCh:        make(chan struct{}, 1),
		stopEpochCh:        make(chan struct{}),
		epochsSinceRecv:    0,
		sentSinceLastEpoch: false,
		connectEpochs:      0,
		backoff:            make(map[int]*backoffState),
		connErrCh:          make(chan error, 1),
		serverStart:        false,
	}

	// start goroutines
	go c.readLoop()
	go c.mainLoop()
	go c.startEpochTimer()
	if err := c.sendConnect(); err != nil {
		_ = c.conn.Close()
		return nil, err
	}

	select {
	case <-c.connAckCh:
		c.serverStart = true
		return c, nil
	case err := <-c.connErrCh:
		_ = c.conn.Close()
		return nil, err
	}
}

func (c *client) ConnID() int {
	return c.connID
}

func (c *client) Read() ([]byte, error) {
	select {
	case p := <-c.appReadCh:
		return p, nil
	case <-c.stopped:
		return nil, errors.New("client closed")
	}
}

func (c *client) Write(payload []byte) error {
	p := append([]byte(nil), payload...)
	select {
	case c.appWriteCh <- p:
		return nil
	case <-c.stopped:
		return errors.New("client closed")
	}
}

func (c *client) Close() error {
	select {
	case c.closeReq <- struct{}{}:
	case <-c.stopped:
	}
	err := <-c.closed
	return err
}

func (c *client) sendConnect() error {
	msg := NewConnect(c.isn)
	return c.sendMsg(msg)
}

func (c *client) sendAck(seq int) {
	if c.connID == 0 {
		return
	}
	_ = c.sendMsg(NewAck(c.connID, seq))
}

func (c *client) sendData(seq int, p []byte) error {
	if c.connID == 0 {
		return errors.New("no connID")
	}
	cs := CalculateChecksum(c.connID, seq, len(p), p)
	msg := NewData(c.connID, seq, len(p), p, cs)
	if err := c.sendMsg(msg); err != nil {
		return err
	}
	c.inflight[seq] = msg
	if _, ok := c.backoff[seq]; !ok {
		c.backoff[seq] = &backoffState{currentBackoff: 0, remaining: 0}
	}
	c.sentSinceLastEpoch = true
	return nil
}

func (c *client) sendMsg(m *Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(b)
	return err
}

func (c *client) oldestUnacked() (int, bool) {
	min := 0
	first := true
	for seq := range c.inflight {
		if first || seq < min {
			min = seq
			first = false
		}
	}
	return min, !first
}

func (c *client) canSendMore() bool {
	if len(c.inflight) >= c.maxUnacked {
		return false
	}
	//making sure we don't go too far past the oldest unacked
	if base, ok := c.oldestUnacked(); ok {
		if c.nextSendSeq-base >= c.windowSize {
			return false
		}
	}
	return true
}

func (c *client) tryDrainSends() {
	for c.connID != 0 && c.canSendMore() && len(c.sendQ) > 0 {
		seq := c.nextSendSeq
		p := c.sendQ[0]
		_ = c.sendData(seq, p)
		c.sendQ = c.sendQ[1:]
		c.nextSendSeq++
	}
}

func (c *client) processIncoming(m Message) {
	c.epochsSinceRecv = 0

	switch m.Type {
	case MsgAck:
		if c.connID == 0 && m.SeqNum == c.isn {
			c.connID = m.ConnID
			c.nextRecvSeq = m.SeqNum + 1
			select {
			case c.connAckCh <- struct{}{}:
			default:
			}
			return
		}
		delete(c.inflight, m.SeqNum)
		delete(c.backoff, m.SeqNum)

	case MsgCAck:
		for s := range c.inflight {
			if s <= m.SeqNum {
				delete(c.inflight, s)
				delete(c.backoff, s)
			}
		}

	case MsgData:
		if m.Size < 0 {
			return
		}
		if len(m.Payload) < m.Size {
			return
		}
		if len(m.Payload) > m.Size {
			m.Payload = m.Payload[:m.Size]
		}
		if CalculateChecksum(c.connID, m.SeqNum, m.Size, m.Payload) != m.Checksum {
			return
		}
		if c.nextRecvSeq == 0 {
			c.nextRecvSeq = m.SeqNum
		}
		if m.SeqNum < c.nextRecvSeq {
			c.sendAck(m.SeqNum)
			return
		}
		if m.SeqNum == c.nextRecvSeq {
			c.deliverQ = append(c.deliverQ, m.Payload)
			c.sendAck(m.SeqNum)
			c.nextRecvSeq++
			for {
				p, ok := c.recvBuf[c.nextRecvSeq]
				if !ok {
					break
				}
				delete(c.recvBuf, c.nextRecvSeq)
				c.deliverQ = append(c.deliverQ, p)
				c.sendAck(c.nextRecvSeq)
				c.nextRecvSeq++
			}
			return
		}
		c.recvBuf[m.SeqNum] = m.Payload
		c.sendAck(m.SeqNum)
	}
}

func (c *client) mainLoop() {
	defer func() {
		close(c.stopped)
	}()

	var outCh chan []byte
	var outNext []byte
	closing := false

	for {
		if len(c.deliverQ) > 0 {
			outCh = c.appReadCh
			outNext = c.deliverQ[0]
		} else {
			outCh = nil
			outNext = nil
		}

		c.tryDrainSends()

		if closing && len(c.sendQ) == 0 && len(c.inflight) == 0 {
			_ = c.conn.Close()
			<-c.readStopped
			c.closed <- nil
			return
		}

		select {
		case p := <-c.appWriteCh:
			c.sendQ = append(c.sendQ, p)

		case m := <-c.netInCh:
			c.processIncoming(m)

		case outCh <- outNext:
			c.deliverQ = c.deliverQ[1:]

		case <-c.closeReq:
			closing = true

		case <-c.epochTickCh:
			if c.connID == 0 {
				_ = c.sendConnect()
				c.connectEpochs++
				if c.connectEpochs >= c.params.EpochLimit {
					select {
					case c.connErrCh <- errors.New("connection timed out"):
					default:
					}
					return
				}
			} else {
				if c.handleEpoch() {
					return
				}
			}
		}
	}
}

func (c *client) handleEpoch() bool {
	resendHappened := false
	for seq, msg := range c.inflight {
		st := c.backoff[seq]
		if st == nil {
			st = &backoffState{currentBackoff: 0, remaining: 0}
			c.backoff[seq] = st
		}
		if st.remaining > 0 {
			st.remaining--
			continue
		}
		_ = c.sendMsg(msg)
		resendHappened = true
		if st.currentBackoff == 0 {
			st.currentBackoff = 1
		} else {
			st.currentBackoff *= 2
			if st.currentBackoff > c.params.MaxBackOffInterval {
				st.currentBackoff = c.params.MaxBackOffInterval
			}
		}
		st.remaining = st.currentBackoff
	}
	if !c.sentSinceLastEpoch && !resendHappened {
		if c.connID != 0 {
			_ = c.sendMsg(NewAck(c.connID, 0))
		}
	}
	c.epochsSinceRecv++
	if c.epochsSinceRecv >= c.params.EpochLimit {
		_ = c.conn.Close()
		<-c.readStopped
		close(c.stopEpochCh)
		c.closed <- errors.New("epoch timeout")
		return true
	}
	c.sentSinceLastEpoch = false
	return false
}
func (c *client) readLoop() {
	defer close(c.readStopped)
	udpBufSize := 65507
	buf := make([]byte, udpBufSize)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			if !c.serverStart {
				continue
			}
			return
		}
		var m Message
		if err := json.Unmarshal(buf[:n], &m); err != nil {
			continue
		}
		select {
		case c.netInCh <- m:
		case <-c.stopped:
			return
		}
	}
}

func (c *client) startEpochTimer() {
	t := time.NewTicker(time.Duration(c.params.EpochMillis) * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			select {
			case c.epochTickCh <- struct{}{}:
			default:
			}
		case <-c.stopEpochCh:
			return
		case <-c.stopped:
			return
		}
	}
}
