package stomp

import (
	"errors"
	"github.com/jjeffery/stomp/frame"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

// Default time span to add to read/write heart-beat timeouts
// to avoid premature disconnections due to network latency.
const DefaultHeartBeatError = 5 * time.Second

// A Conn is a connection to a STOMP server. Create a Conn using either
// the Dial or Connect function.
type Conn struct {
	conn         io.ReadWriteCloser
	readCh       chan *Frame
	writeCh      chan writeRequest
	version      Version
	session      string
	server       string
	readTimeout  time.Duration
	writeTimeout time.Duration
	options      *connOptions
}

type writeRequest struct {
	Frame *Frame      // frame to send
	C     chan *Frame // response channel
}

// Dial creates a network connection to a STOMP server and performs
// the STOMP connect protocol sequence. The network endpoint of the
// STOMP server is specified by network and addr. STOMP protocol
// options can be specified in opts.
func Dial(network, addr string, opts ...func(*Conn) error) (*Conn, error) {
	c, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		c.Close()
		return nil, err
	}

	// Add option to set host and make it the first option in list,
	// so that if host has been explicitly specified it will override.
	opts = append([](func(*Conn) error){ConnOpt.Host(host)}, opts...)

	return Connect(c, opts...)
}

// Connect creates a STOMP connection and performs the STOMP connect
// protocol sequence. The connection to the STOMP server has already
// been created by the program. The opts parameter provides the
// opportunity to specify STOMP protocol options.
func Connect(conn io.ReadWriteCloser, opts ...func(*Conn) error) (*Conn, error) {
	reader := NewReader(conn)
	writer := NewWriter(conn)

	c := &Conn{
		conn:    conn,
		readCh:  make(chan *Frame, 8),
		writeCh: make(chan writeRequest, 8),
	}

	options, err := newConnOptions(c, opts)
	if err != nil {
		return nil, err
	}

	if options.Host == "" {
		// host not specified yet, attempt to get from net.Conn if possible
		if connection, ok := conn.(net.Conn); ok {
			host, _, err := net.SplitHostPort(connection.RemoteAddr().String())
			if err == nil {
				options.Host = host
			}
		}
		// if host is still blank, use default
		if options.Host == "" {
			options.Host = "default"
		}
	}

	connectFrame, err := options.NewFrame()
	if err != nil {
		return nil, err
	}

	err = writer.Write(connectFrame)
	if err != nil {
		return nil, err
	}

	response, err := reader.Read()
	if err != nil {
		return nil, err
	}

	if response.Command != frame.CONNECTED {
		return nil, newError(response)
	}

	c.server = response.Get(frame.Server)
	c.session = response.Get(frame.Session)

	if versionString := response.Get(frame.Version); versionString != "" {
		version := Version(versionString)
		if err = version.CheckSupported(); err != nil {
			return nil, Error{
				Message: err.Error(),
				Frame:   response,
			}
		}
		c.version = version
	} else {
		// no version in the response, so assume version 1.0
		c.version = V10
	}

	if heartBeat, ok := response.Contains(frame.HeartBeat); ok {
		readTimeout, writeTimeout, err := frame.ParseHeartBeat(heartBeat)
		if err != nil {
			return nil, Error{
				Message: err.Error(),
				Frame:   response,
			}
		}

		c.readTimeout = readTimeout
		c.writeTimeout = writeTimeout

		if c.readTimeout > 0 {
			// Add time to the read timeout to account for time
			// delay in other station transmitting timeout
			c.readTimeout += options.HeartBeatError
		}
		if c.writeTimeout > options.HeartBeatError {
			// Reduce time from the write timeout to account
			// for time delay in transmitting to the other station
			c.writeTimeout -= options.HeartBeatError
		}
	}

	// TODO(jpj): make any non-standard headers in the CONNECTED
	// frame available. This could be implemented as:
	// (a) a callback function supplied as an option; or
	// (b) a property of the Conn structure (eg CustomHeaders)
	// Neither options are particularly elegant, so wait until
	// there is a real need for this.

	go readLoop(c, reader)
	go processLoop(c, writer)

	return c, nil
}

// Version returns the version of the STOMP protocol that
// is being used to communicate with the STOMP server. This
// version is negotiated with the server during the connect sequence.
func (c *Conn) Version() Version {
	return c.version
}

// Session returns the session identifier, which can be
// returned by the STOMP server during the connect sequence.
// If the STOMP server does not return a session header entry,
// this value will be a blank string.
func (c *Conn) Session() string {
	return c.session
}

// Server returns the STOMP server identification, which can
// be returned by the STOMP server during the connect sequence.
// If the STOMP server does not return a server header entry,
// this value will be a blank string.
func (c *Conn) Server() string {
	return c.server
}

// readLoop is a goroutine that reads frames from the
// reader and places them onto a channel for processing
// by the processLoop goroutine
func readLoop(c *Conn, reader *Reader) {
	for {
		f, err := reader.Read()
		if err != nil {
			close(c.readCh)
			return
		}
		c.readCh <- f
	}
}

// processLoop is a goroutine that handles io with
// the server.
func processLoop(c *Conn, writer *Writer) {
	channels := make(map[string]chan *Frame)

	var readTimeoutChannel <-chan time.Time
	var readTimer *time.Timer
	var writeTimeoutChannel <-chan time.Time
	var writeTimer *time.Timer

	for {
		if c.readTimeout > 0 && readTimer == nil {
			readTimer := time.NewTimer(c.readTimeout)
			readTimeoutChannel = readTimer.C
		}
		if c.writeTimeout > 0 && writeTimer == nil {
			writeTimer := time.NewTimer(c.writeTimeout)
			writeTimeoutChannel = writeTimer.C
		}

		select {
		case <-readTimeoutChannel:
			// read timeout, close the connection
			err := newErrorMessage("read timeout")
			sendError(channels, err)
			return

		case <-writeTimeoutChannel:
			// write timeout, send a heart-beat frame
			err := writer.Write(nil)
			if err != nil {
				sendError(channels, err)
				return
			}
			writeTimer = nil
			writeTimeoutChannel = nil

		case f, ok := <-c.readCh:
			// stop the read timer
			if readTimer != nil {
				readTimer.Stop()
				readTimer = nil
				readTimeoutChannel = nil
			}

			if !ok {
				err := newErrorMessage("connection closed")
				sendError(channels, err)
				return
			}

			if f == nil {
				// heart-beat received
				continue
			}

			switch f.Command {
			case frame.RECEIPT:
				if id, ok := f.Contains(frame.ReceiptId); ok {
					if ch, ok := channels[id]; ok {
						ch <- f
						delete(channels, id)
						close(ch)
					}

				} else {
					err := &Error{Message: "missing receipt-id", Frame: f}
					sendError(channels, err)
					return
				}

			case frame.ERROR:
				log.Println("received ERROR")
				for _, ch := range channels {
					ch <- f
					close(ch)
				}

				return

			case frame.MESSAGE:
				if id, ok := f.Contains(frame.Subscription); ok {
					if ch, ok := channels[id]; ok {
						ch <- f
					} else {
						log.Println("ignored MESSAGE for subscription", id)
					}
				}
			}

		case req, ok := <-c.writeCh:
			// stop the write timeout
			if writeTimer != nil {
				writeTimer.Stop()
				writeTimer = nil
				writeTimeoutChannel = nil
			}
			if !ok {
				sendError(channels, errors.New("write channel closed"))
				return
			}
			if req.C != nil {
				if receipt, ok := req.Frame.Contains(frame.Receipt); ok {
					// remember the channel for this receipt
					channels[receipt] = req.C
				}
			}

			switch req.Frame.Command {
			case frame.SUBSCRIBE:
				id, _ := req.Frame.Contains(frame.Id)
				channels[id] = req.C
			case frame.UNSUBSCRIBE:
				id, _ := req.Frame.Contains(frame.Id)
				// is this trying to be too clever -- add a receipt
				// header so that when the server responds with a
				// RECEIPT frame, the corresponding channel will be closed
				req.Frame.Set(frame.Receipt, id)
			}

			// frame to send
			err := writer.Write(req.Frame)
			if err != nil {
				sendError(channels, err)
				return
			}
		}
	}
}

// Send an error to all receipt channels.
func sendError(m map[string]chan *Frame, err error) {
	frame := NewFrame(frame.ERROR, frame.Message, err.Error())
	for _, ch := range m {
		ch <- frame
	}
}

// Disconnect will disconnect from the STOMP server. This function
// follows the STOMP standard's recommended protocol for graceful
// disconnection: it sends a DISCONNECT frame with a receipt header
// element. Once the RECEIPT frame has been received, the connection
// with the STOMP server is closed and any further attempt to write
// to the server will fail.
func (c *Conn) Disconnect() error {
	ch := make(chan *Frame)
	c.writeCh <- writeRequest{
		Frame: NewFrame(frame.DISCONNECT, frame.Receipt, allocateId()),
		C:     ch,
	}

	response := <-ch
	if response.Command != frame.RECEIPT {
		return newError(response)
	}

	return c.conn.Close()
}

// Send sends a message to the STOMP server, which in turn sends the message to the specified destination.
// This method returns without confirming that the STOMP server has received the message. If the STOMP server
// does fail to receive the message for any reason, the connection will close.
//
// The content type should be specified, according to the STOMP specification, but if contentType is an empty
// string, the message will be delivered without a content type header entry. The body array contains the
// message body, and its content should be consistent with the specified content type.
//
// The message can contain optional, user-defined header entries in userDefined. If there are no optional header
// entries, then set userDefined to nil.
func (c *Conn) Send(destination, contentType string, body []byte, userDefined *Header) error {
	// TODO(jpj): Check that we are still connected before sending.
	f := createSendFrame(destination, contentType, body, userDefined)
	f.Del(frame.Transaction)
	c.sendFrame(f)
	return nil
}

// Send sends a message to the STOMP server, which in turn sends the message to the specified destination.
// This method does not return until the STOMP server has confirmed receipt of the message.
//
// The content type should be specified, according to the STOMP specification, but if contentType is an empty
// string, the message will be delivered without a content type header entry. The body array contains the
// message body, and its content should be consistent with the specified content type.
//
// The message can contain optional, user-defined header entries in userDefined. If there are no optional header
// entries, then set userDefined to nil.
func (c *Conn) SendWithReceipt(destination, contentType string, body []byte, userDefined *Header) error {
	// TODO(jpj): Check that we are still connected before sending.
	f := createSendFrame(destination, contentType, body, userDefined)
	f.Del(frame.Transaction)
	return c.sendFrameWithReceipt(f)
}

func createSendFrame(destination, contentType string, body []byte, userDefined *Header) *Frame {
	f := &Frame{
		Command: frame.SEND,
		Body:    body,
	}
	if userDefined == nil {
		f.Header = NewHeader()
	} else {
		f.Header = userDefined.Clone()
		f.Header.Del(frame.Receipt)
	}

	f.Header.Set(frame.Destination, destination)

	if contentType == "" {
		// no content type specified
		f.Header.Del(frame.ContentType)
	} else {
		f.Header.Set(frame.ContentType, contentType)
	}

	f.Header.Set(frame.ContentLength, strconv.Itoa(len(body)))
	return f
}

func (c *Conn) sendFrame(f *Frame) {
	request := writeRequest{Frame: f}
	c.writeCh <- request
}

func (c *Conn) sendFrameWithReceipt(f *Frame) error {
	receipt := allocateId()
	f.Set(frame.Receipt, receipt)

	request := writeRequest{Frame: f}

	request.C = make(chan *Frame)
	c.writeCh <- request
	response := <-request.C
	if response.Command != frame.RECEIPT {
		return newError(response)
	}
	// TODO(jpj) Check receipt id

	return nil
}

// Subscribe creates a subscription on the STOMP server.
// The subscription has a destination, and messages sent to that destination
// will be received by this subscription. A subscription has a channel
// on which the calling program can receive messages.
func (c *Conn) Subscribe(destination string, ack AckMode) (*Subscription, error) {
	return c.SubscribeWithHeaders(destination, ack, nil)
}

// SubscribeWithHeaders is similar to Subscribe, but also sends optional headers with
// the subscription.
func (c *Conn) SubscribeWithHeaders(destination string, ack AckMode, headers *Header) (*Subscription, error) {
	ch := make(chan *Frame)
	id := allocateId()

	subscribeFrame := NewFrame(frame.SUBSCRIBE,
		frame.Id, id,
		frame.Destination, destination,
		frame.Ack, ack.String())

	if headers != nil {

		for i := 0; i < headers.Len(); i++ {
			key, value := headers.GetAt(i)

			subscribeFrame.Add(key, value)
		}

	}

	request := writeRequest{
		Frame: subscribeFrame,
		C:     ch,
	}

	sub := &Subscription{
		id:          id,
		destination: destination,
		conn:        c,
		ackMode:     ack,
		C:           make(chan *Message, 16),
	}
	go sub.readLoop(ch)

	c.writeCh <- request
	return sub, nil
}

// Ack acknowledges a message received from the STOMP server.
// If the message was received on a subscription with AckMode == AckAuto,
// then no operation is performed.
func (c *Conn) Ack(m *Message) error {
	f, err := c.createAckNackFrame(m, true)
	if err != nil {
		return err
	}

	if f != nil {
		c.sendFrame(f)
	}
	return nil
}

func (c *Conn) Nack(m *Message) error {
	f, err := c.createAckNackFrame(m, false)
	if err != nil {
		return err
	}

	if f != nil {
		c.sendFrame(f)
	}
	return nil
}

// Begin is used to start a transaction. Transactions apply to sending
// and acknowledging. Any messages sent or acknowledged during a transaction
// will be processed atomically by the STOMP server based on the transaction.
func (c *Conn) Begin() *Transaction {
	id := allocateId()
	f := NewFrame(frame.BEGIN, frame.Transaction, id)
	c.sendFrame(f)
	return &Transaction{id: id, conn: c}
}

// Create an ACK or NACK frame. Complicated by version incompatibilities.
func (c *Conn) createAckNackFrame(msg *Message, ack bool) (*Frame, error) {
	if !ack && !c.version.SupportsNack() {
		return nil, nackNotSupported
	}

	if msg.Header == nil || msg.Subscription == nil || msg.Conn == nil {
		return nil, notReceivedMessage
	}

	if msg.Subscription.AckMode() == AckAuto {
		if ack {
			// not much point sending an ACK to an auto subscription
			return nil, nil
		} else {
			// sending a NACK for an ack:auto subscription makes no
			// sense
			return nil, cannotNackAutoSub
		}
	}

	var f *Frame
	if ack {
		f = NewFrame(frame.ACK)
	} else {
		f = NewFrame(frame.NACK)
	}

	switch c.version {
	case V10, V11:
		f.Header.Add(frame.Subscription, msg.Subscription.Id())
		if messageId, ok := msg.Header.Contains(frame.MessageId); ok {
			f.Header.Add(frame.MessageId, messageId)
		} else {
			return nil, missingHeader(frame.MessageId)
		}
	case V12:
		if ack, ok := msg.Header.Contains(frame.Ack); ok {
			f.Header.Add(frame.Id, ack)
		} else {
			return nil, missingHeader(frame.Ack)
		}
	}

	return f, nil
}
