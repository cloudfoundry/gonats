package nats

import (
	"net"
	"sync"
)

type Subscription struct {
	sr *subscriptionRegistry

	sid      uint
	frozen   bool
	maximum  uint
	received uint

	subject string
	queue   string

	Inbox chan *readMessage
}

func (s *Subscription) freeze() {
	if s.frozen {
		panic("subscription is frozen")
	}

	s.frozen = true
}

func (s *Subscription) SetSubject(v string) {
	if s.frozen {
		panic("subscription is frozen")
	}

	s.subject = v
}

func (s *Subscription) SetQueue(v string) {
	if s.frozen {
		panic("subscription is frozen")
	}

	s.queue = v
}

func (s *Subscription) SetMaximum(v uint) {
	if s.frozen {
		panic("subscription is frozen")
	}

	s.maximum = v
}

func (s *Subscription) writeSubscribe() writeObject {
	var o = new(writeSubscribe)

	o.Sid = s.sid
	o.Subject = s.subject
	o.Queue = s.queue

	return o
}

func (s *Subscription) writeUnsubscribe(includeMaximum bool) writeObject {
	var o = new(writeUnsubscribe)

	o.Sid = s.sid

	if includeMaximum {
		o.Maximum = s.maximum
	}

	return o
}

func (s *Subscription) Subscribe() {
	s.sr.Subscribe(s)
}

func (s *Subscription) Unsubscribe() {
	s.sr.Unsubscribe(s)
}

func (s *Subscription) deliver(m *readMessage) {
	s.received++
	s.Inbox <- m

	// Unsubscribe if the maximum number of messages has been received
	if s.maximum > 0 && s.received >= s.maximum {
		s.Unsubscribe()
	}
}

type subscriptionRegistry struct {
	sync.Mutex
	*Client

	sid uint
	m   map[uint]*Subscription
}

func (sr *subscriptionRegistry) emptyMap() {
	sr.m = make(map[uint]*Subscription)
}

func (sr *subscriptionRegistry) setup(c *Client) {
	sr.Client = c

	sr.emptyMap()
}

func (sr *subscriptionRegistry) teardown() {
	sr.Lock()
	defer sr.Unlock()

	for _, s := range sr.m {
		close(s.Inbox)
	}

	sr.emptyMap()
}

func (sr *subscriptionRegistry) NewSubscription(sub string) *Subscription {
	var s = new(Subscription)

	sr.Lock()

	s.sr = sr

	sr.sid++
	s.sid = sr.sid

	sr.Unlock()

	s.SetSubject(sub)
	s.Inbox = make(chan *readMessage)

	return s
}

func (sr *subscriptionRegistry) Subscribe(s *Subscription) {
	sr.Lock()

	sr.m[s.sid] = s
	s.freeze()

	sr.Unlock()

	sr.Client.Write(s.writeSubscribe())

	if s.maximum > 0 {
		sr.Client.Write(s.writeUnsubscribe(true))
	}

	return
}

func (sr *subscriptionRegistry) Unsubscribe(s *Subscription) {
	sr.Lock()

	delete(sr.m, s.sid)

	// Since this subscription is now removed from the registry, it will no
	// longer receive messages and the inbox can be closed
	close(s.Inbox)

	sr.Unlock()

	sr.Client.Write(s.writeUnsubscribe(false))

	return
}

func (sr *subscriptionRegistry) Deliver(m *readMessage) {
	var s *Subscription
	var ok bool

	sr.Lock()
	s, ok = sr.m[m.SubscriptionId]
	sr.Unlock()

	if ok {
		s.deliver(m)
	}
}

type Client struct {
	subscriptionRegistry

	cc chan *Connection

	// Notify running client to stop
	sc chan bool
}

func NewClient() *Client {
	var t = new(Client)

	t.subscriptionRegistry.setup(t)

	t.cc = make(chan *Connection)

	t.sc = make(chan bool)

	return t
}

func (t *Client) AcquireConnection() *Connection {
	var c *Connection
	var ok bool

	c, ok = <-t.cc
	if !ok {
		return nil
	}

	return c
}

func (t *Client) Write(o writeObject) bool {
	c := t.AcquireConnection()
	if c == nil {
		return false
	}

	return c.Write(o)
}

func (t *Client) Ping() bool {
	c := t.AcquireConnection()
	if c == nil {
		return false
	}

	return c.Ping()
}

func (t *Client) publish(s string, m []byte, confirm bool) bool {
	var o = new(writePublish)

	o.Subject = s
	o.Message = m

	c := t.AcquireConnection()
	if c == nil {
		return false
	}

	ok := c.Write(o)
	if !ok {
		return false
	}

	// Round trip to confirm the publish was received
	if confirm {
		return c.Ping()
	}

	return true
}

func (t *Client) Publish(s string, m []byte) bool {
	return t.publish(s, m, false)
}

func (t *Client) PublishAndConfirm(s string, m []byte) bool {
	return t.publish(s, m, true)
}

func (t *Client) Stop() {
	t.sc <- true
}

func (t *Client) runConnection(n net.Conn) error {
	var e error
	var c *Connection
	var dc chan bool

	c = NewConnection(n)
	dc = make(chan bool)

	// Feed connection until stop
	go func() {
		for {
			select {
			case t.cc <- c:
			case <-t.sc:
				c.Stop()

				// Wait for c.Run() to return and notify dc
				<-dc
				return

			case <-dc:
				return
			}
		}
	}()

	// Read messages until EOF
	go func() {
		var o readObject

		for o = range c.oc {
			switch oo := o.(type) {
			case *readMessage:
				t.Deliver(oo)
			}
		}
	}()

	e = c.Run()
	dc <- true

	return e
}

func (t *Client) Run(d Dialer, h Handshaker) error {
	var n net.Conn
	var e error

	// There will not be more connections after Run returns
	defer close(t.cc)

	// There will not be more messages after Run returns
	defer t.subscriptionRegistry.teardown()

	for {
		n, e = d.Dial()
		if e != nil {
			// Error: dialer couldn't establish a connection
			return e
		}

		n, e = h.Handshake(n)
		if e != nil {
			// Error: handshake couldn't complete
			return e
		}

		e = t.runConnection(n)
		if e == nil {
			// No error: client was explicitly stopped
			return nil
		}
	}

	return nil
}

func (t *Client) RunWithDefaults(addr string, user, pass string) error {
	d := DefaultDialer(addr)
	h := DefaultHandshaker(user, pass)
	return t.Run(d, h)
}
