// Copyright 2020 lesismal. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package arpc

import (
	"bufio"
	"fmt"
	"io"
	"net"
)

// DefaultHandler instance
var DefaultHandler Handler = NewHandler()

// HandlerFunc type define
type HandlerFunc func(*Context)

// Handler defines net message handler
type Handler interface {
	// Clone returns a copy
	Clone() Handler

	// BeforeRecv registers callback before Recv
	BeforeRecv(bh func(net.Conn) error)

	// BeforeSend registers callback before Send
	BeforeSend(bh func(net.Conn) error)

	// BatchRecv flag
	BatchRecv() bool
	// SetBatchRecv flag
	SetBatchRecv(batch bool)
	// BatchSend flag
	BatchSend() bool
	// SetBatchSend flag
	SetBatchSend(batch bool)

	// WrapReader wraps net.Conn to Read data with io.Reader, buffer e.g.
	WrapReader(conn net.Conn) io.Reader
	// SetReaderWrapper sets reader wrapper
	SetReaderWrapper(wrapper func(conn net.Conn) io.Reader)

	// Recv reads and returns a message from a client
	Recv(c *Client) (Message, error)

	// Send writes a message to a connection
	Send(c net.Conn, m Message) (int, error)
	// SendN writes batch messages to a connection
	SendN(conn net.Conn, buffers net.Buffers) (int, error)

	// SendQueueSize returns Client's chSend capacity
	SendQueueSize() int
	// SetSendQueueSize sets Client's chSend capacity
	SetSendQueueSize(size int)

	// Handle registers method handler
	Handle(m string, h HandlerFunc)

	// OnMessage dispatches messages
	OnMessage(c *Client, m Message)
}

type handler struct {
	batchRecv     bool
	batchSend     bool
	sendQueueSize int

	beforeRecv func(net.Conn) error
	beforeSend func(net.Conn) error

	wrapReader func(conn net.Conn) io.Reader

	routes map[string]HandlerFunc
}

// Clone returns a copy
func (h *handler) Clone() Handler {
	var cp = *h
	return &cp
}

func (h *handler) BeforeRecv(bh func(net.Conn) error) {
	h.beforeRecv = bh
}

func (h *handler) BeforeSend(bh func(net.Conn) error) {
	h.beforeSend = bh
}

// BatchRecv flag
func (h *handler) BatchRecv() bool {
	return h.batchRecv
}

// SetBatchRecv flag
func (h *handler) SetBatchRecv(batch bool) {
	h.batchRecv = batch
}

// BatchSend flag
func (h *handler) BatchSend() bool {
	return h.batchSend
}

// SetBatchSend flag
func (h *handler) SetBatchSend(batch bool) {
	h.batchSend = batch
}

func (h *handler) WrapReader(conn net.Conn) io.Reader {
	if h.wrapReader != nil {
		return h.wrapReader(conn)
	}
	return conn
}

func (h *handler) SetReaderWrapper(wrapper func(conn net.Conn) io.Reader) {
	h.wrapReader = wrapper
}

func (h *handler) SendQueueSize() int {
	return h.sendQueueSize
}

func (h *handler) SetSendQueueSize(size int) {
	h.sendQueueSize = size
}

func (h *handler) Handle(method string, cb HandlerFunc) {
	if h.routes == nil {
		h.routes = map[string]HandlerFunc{}
	}
	if len(method) > MaxMethodLen {
		panic(fmt.Errorf("invalid method length %v(> MaxMethodLen %v)", len(method), MaxMethodLen))
	}
	if _, ok := h.routes[method]; ok {
		panic(fmt.Errorf("handler exist for method %v ", method))
	}
	h.routes[method] = cb
}

func (h *handler) Recv(c *Client) (Message, error) {
	var (
		err     error
		message Message
	)

	if h.beforeRecv != nil {
		if err = h.beforeRecv(c.Conn); err != nil {
			return nil, err
		}
	}

	_, err = io.ReadFull(c.Reader, c.Head)
	if err != nil {
		return nil, err
	}

	message, err = c.Head.message()
	if err == nil && len(message) > HeadLen {
		_, err = io.ReadFull(c.Reader, message[HeadLen:])
	}

	return message, err
}

func (h *handler) Send(conn net.Conn, m Message) (int, error) {
	if h.beforeSend != nil {
		if err := h.beforeSend(conn); err != nil {
			return -1, err
		}
	}
	return conn.Write(m)
}

func (h *handler) SendN(conn net.Conn, buffers net.Buffers) (int, error) {
	if h.beforeSend != nil {
		if err := h.beforeSend(conn); err != nil {
			return -1, err
		}
	}
	n64, err := buffers.WriteTo(conn)
	return int(n64), err
}

func (h *handler) OnMessage(c *Client, msg Message) {
	// cmd, seq, isAsync, method, body, err := msg.parse()
	switch msg.Cmd() {
	case CmdRequest, CmdNotify:
		if msg.MethodLen() == 0 {
			DefaultLogger.Warn("OnMessage: invalid request message with 0 method length, dropped")
			return
		}
		method := msg.Method()
		if handler, ok := h.routes[method]; ok {
			ctx := ctxGet(c, msg)
			defer func() {
				ctxPut(ctx)
				memPut(msg)
			}()
			defer handlePanic()
			handler(ctx)
		} else {
			memPut(msg)
			DefaultLogger.Warn("OnMessage: invalid method: [%v], no handler", method)
		}
	case CmdResponse:
		if msg.MethodLen() > 0 {
			DefaultLogger.Warn("OnMessage: invalid response message with method length %v, dropped", msg.MethodLen())
			return
		}
		if !msg.IsAsync() {
			seq := msg.Seq()
			session, ok := c.getSession(seq)
			if ok {
				session.done <- msg
			} else {
				memPut(msg)
				DefaultLogger.Warn("OnMessage: session not exist or expired")
			}
		} else {
			handler, ok := c.getAndDeleteAsyncHandler(msg.Seq())
			if ok {
				ctx := ctxGet(c, msg)
				defer func() {
					ctxPut(ctx)
					memPut(msg)
				}()
				defer handlePanic()
				handler(ctx)
			} else {
				memPut(msg)
				DefaultLogger.Warn("OnMessage: async handler not exist or expired")
			}
		}
	default:
		memPut(msg)
		DefaultLogger.Info("OnMessage: invalid cmd [%v]", msg.Cmd())
	}
}

// NewHandler factory
func NewHandler() Handler {
	return &handler{
		batchRecv:     true,
		batchSend:     true,
		sendQueueSize: 1024,
		wrapReader: func(conn net.Conn) io.Reader {
			return bufio.NewReaderSize(conn, 1024)
		},
	}
}
