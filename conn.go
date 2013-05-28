package main

import (
	"io/ioutil"
	"log"
	"sync"
	"time"

	"github.com/garyburd/go-websocket/websocket"
)

type connection struct {
	//The websocket connection.
	ws *websocket.Conn

	//Buffered channel of outbound messages.
	//If the buffer is reached, the client will be
	//considered to have timed out and disconnected.
	//This can really only happen if message order is not preserved.
	send chan []byte

	//Have we received a kill signal?
	dead bool

	//We need to lock the connection, since it can be
	//shared by multiple hubs (in theory), or have multiple
	//goroutines accessing it from multiple simultaneous goroutines
	mu sync.RWMutex
}

//connection.Send is the interface that hubs and other instruments are allowed to
//use to send a message to the user at the other end of this websocket connection
//The hub is notified when finished by sending an empty struct over the fin channel
func (c *connection) Send(message []byte, fin chan struct{}, h *hub) {
	defer func() {
		log.Printf("conn.Send: message '''%s''' to %v\n", string(message), c)

		//Tell the calling function that this goroutine is done sending
		fin <- struct{}{}
	}()

	c.mu.RLock()
	if c.dead {
		//Channel is already dead, we cannot send on it anymore and we must exit
		c.mu.RUnlock()
		return
	}
	c.mu.RUnlock()

	//We don't want to try to send over the channel if another
	//goroutine has closed this channel in the meantime. Thus, we
	//must block writing before we send over this channel.
	c.mu.Lock()
	select {
	//If the message is sent over the websocket, unlock this connection and continue
	case c.send <- message:
		c.mu.Unlock()

	//If we cannot send, this means that the user's buffer is full. At this point we basically
	//assume that the user disconnected or is just stuck.
	default:
		//Tell the hub to unregister us, close the send channel, and close the websocket
		log.Printf("conn.Send: Implied disconnect of %+v\n", c)
		//Unlock before unregistering since the act of unregistering triggers changes in c
		c.mu.Unlock()
		h.unregister <- c
	}
}

//connection.reader passes messages from the user to the hub for broadcasting.
//It also handles the 'pong' portion of ping-pong keepalives.
func (c *connection) reader(h *hub) {
	//Shouldn't need to c.ws.Close() here because ultimately
	// this will cause the deferred unregister in wsHandler() to fire
	//defer c.ws.Close()
	defer log.Printf("conn.reader: reader for %+v exited\n", c)
	c.ws.SetReadLimit(maxMessageSize)
	c.ws.SetReadDeadline(time.Now().Add(readWait))
	for {
		op, r, err := c.ws.NextReader()
		if err != nil {
			log.Printf("conn.reader: Error trying to broadcast the message: %+v, %+v, %s\n", op, r, err)
			break
		}

		switch op {
		case websocket.OpPong:
			c.ws.SetReadDeadline(time.Now().Add(readWait))
		case websocket.OpText:
			message, err := ioutil.ReadAll(r)
			if err != nil {
				break
			}
			//Send the message to the hub
			h.broadcast <- message
		}
	}
}

//connection.write actually sends a message with the given opCode and payload
//down the wire to the user.
func (c *connection) write(opCode int, payload []byte) error {
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteMessage(opCode, payload)
}

//connection.writer waits to send messages that were broadcast to this
//particular connection down the wire to the user. If none is received
//in time, it sends a ping for connection keepalive. In this way, timeouts
//are managed on a per-connection basis.
func (c *connection) writer() {
	//Shouldn't need to c.ws.Close() here because ultimately
	// this will cause the deferred unregister in wsHandler() to fire
	//defer c.ws.Close()
	defer log.Printf("conn.writer: writer for %+v exited\n", c)

	ticker := time.NewTicker(pingPeriod)
	defer func() { ticker.Stop() }()

	for {
		select {
		//Client will get a message
		case message, ok := <-c.send:
			if !ok {
				c.write(websocket.OpClose, []byte{})
				return
			}
			if err := c.write(websocket.OpText, message); err != nil {
				return
			}
		//Client isn't getting a message in time to keep them alive, so
		// send a ping
		case <-ticker.C:
			if err := c.write(websocket.OpPing, []byte{}); err != nil {
				return
			}
		}
	}
}
