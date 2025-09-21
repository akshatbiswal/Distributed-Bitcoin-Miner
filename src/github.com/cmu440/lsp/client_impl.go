// Contains the implementation of a LSP client.

package lsp

import (
	"errors"
	"github.com/cmu440/lspnet"
)

type client struct {
	 // fixed
	 conn         *lspnet.UDPConn
	 serverAddr   *lspnet.UDPAddr
	 params       *Params
 
	 // ids & seq nums
	 connID       int                 // after connect ACK
	 isn          int                 // initial seq num
	 nextSendSeq  int                 // starts at isn+1 (client->server stream)
	 nextRecvSeq  int                 // expected next seq from server
 
	 // send-side state (sliding window)
	 windowSize   int
	 maxUnacked   int
	 inflight     map[int]*Message    // seq -> msg (unACKed)
	 sendQ        [][]byte            // app payloads waiting for window room
	 backoff      map[int]int         // seq -> current backoff (epochs since last xmit and target)
 
	 // recv-side reordering
	 recvBuf      map[int][]byte      // seq -> payload (arrived > expected)
	 appReadQ     chan []byte         // deliver to Read in-order
 
	 // control channels
	 appWriteQ    chan []byte         // Write() -> main loop
	 closeReq     chan struct{}       // Close() -> main loop
	 closed       chan error          // main loop -> Close() completion
 
	 // epoch/timeouts
	 epochsSinceHeard int             // for connection loss detection
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
	c := &client{conn: conn,}
	return c, nil
	
}

func (c *client) ConnID() int {
	return c.connID
}

func (c *client) Read() ([]byte, error) {
	// TODO: remove this line when you are ready to begin implementing this method.
	select {} // Blocks indefinitely.
	return nil, errors.New("not yet implemented")
}

func (c *client) Write(payload []byte) error {
	return errors.New("not yet implemented")
}

func (c *client) Close() error {
	return errors.New("not yet implemented")
}
