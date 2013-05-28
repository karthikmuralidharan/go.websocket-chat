package main

import (
	"fmt"
	"log"
	"sync"
)

type hubMap struct {
	m  map[string](*hub)
	mu sync.RWMutex
}

func (all *hubMap) BroadcastAll(input []byte) {
	all.mu.Lock()
	defer all.mu.Unlock()

	for _, h := range all.m {
		h.broadcast <- input
	}

	return
}

func GetHub(id string) *hub {
	hubs.mu.RLock()

	//Hub has already been created
	if hubs.m[id] != nil {
		defer hubs.mu.RUnlock()
		log.Printf("GetHub: hub %s already exists\n", id)
		return hubs.m[id]
	}
	hubs.mu.RUnlock()

	//Hub has not been created
	hubs.mu.Lock()
	defer hubs.mu.Unlock()

	h := &hub{
		id:          id,
		broadcast:   make(chan []byte, 256), //Guarantee up to 256 messages in order
		register:    make(chan *connection),
		unregister:  make(chan *connection),
		connections: connectionMap{m: make(map[*connection]struct{})},
	}
	log.Printf("hubs.m is %+v", hubs.m)
	hubs.m[id] = h
	go h.run()
	log.Printf("GetHub: new hub %s created\n", id)
	return h
}

type connectionMap struct {
	m  map[*connection]struct{}
	mu sync.RWMutex
	//exists bool
}

type hub struct {
	id string

	// Registered connections.
	connections connectionMap

	// Inbound messages from the connections.
	//The buffer, if any, guarantees the number of
	//messages which will be received by every client in order
	broadcast chan []byte

	// Register requests from the connections.
	register chan *connection

	// Unregister requests from connections.
	unregister chan *connection
}

func (h *hub) run() {
	for {
		select {
		case connection := <-h.register:
			//Add a connection
			go h.connect(connection)
		case connection := <-h.unregister:
			//Delete a connection
			go h.disconnect(connection)
		case message := <-h.broadcast:
			//We've received a message that is potentially supposed to be broadcast

			//If not a goroutine messages will be received by each client in order
			//(unless 1: there is a goroutine internally, or 2: hub.broadcast is unbuffered or is over its buffer)
			//If a goroutine, no guarantee about message order
			h.bcast(message)
		}
	}
}

func (h *hub) connect(connection *connection) {
	h.connections.mu.Lock()
	h.connections.m[connection] = struct{}{}
	numCons := len(h.connections.m)
	h.connections.mu.Unlock()

	//Unless register and unregister have a buffer, make sure any messaging during these
	//processes is concurrent.
	go func() {
		h.broadcast <- []byte(fmt.Sprintf("hub.connect: %v connected", connection))
		h.broadcast <- []byte(fmt.Sprintf("%d clients currently connected to hub %s\n", numCons, h.id))
	}()
	log.Printf("hub.connect: %v connected\n", connection)
	log.Printf("hub.connect: %d clients currently connected\n", numCons)
}

func (h *hub) disconnect(connection *connection) {
	//could wrap these in goroutines with semaphores to make sure
	//that hub.disconnect() doesn't return until both goroutines are
	//done
	h.connections.mu.Lock()
	delete(h.connections.m, connection)
	numCons := len(h.connections.m)
	h.connections.mu.Unlock()

	connection.mu.Lock()
	connection.dead = true
	close(connection.send)
	connection.ws.Close()
	connection.mu.Unlock()

	//Unless register and unregister have a buffer, make sure any messaging during these
	//processes is concurrent.
	if numCons > 0 {
		go func() {
			h.broadcast <- []byte(fmt.Sprintf("hub.disconnect: %v disconnected", connection))
			h.broadcast <- []byte(fmt.Sprintf("%d clients currently connected to hub %s\n", numCons, h.id))
			log.Printf("\nhub.disconnect: FINAL NOTICE %v disconnected FINAL NOTICE\n", connection)
			log.Printf("hub.connect: %d clients currently connected\n", numCons)
		}()
	} else {
		defer func() {
			hubs.mu.Lock()
			defer func() { hubs.mu.Unlock() }()
			delete(hubs.m, h.id)
			
			log.Printf("hub.disconnect: these hubs now exist: %+v\n", hubs.m) 
		}()
	}
}

func (h *hub) bcast(message []byte) {
	//RLock here would guarantee that the map won't change while we iterate over it BUT other goroutines
	// could read the next message simultaneously, so message order is not guaranteed. However, concurrency
	// is maximized.
	//Lock here would guarantee that the map won't change while we iterate over it AND that
	// this is the only goroutine currently reading the map (i.e., it would preserve message order). The
	// degree to which concurrency is impaired depends on whether conn.Send() is called as a goroutine or not.
	//If conn.Send() is called as a goroutine, then choosing between Lock or RLock is of minimal importance,
	// as they would both protect the map just until each connection was launched (but not finished).
	//If conn.Send() is called as a normal routine, then
	h.connections.mu.RLock()

	//Count launched routines
	i := 0
	finChan := make(chan struct{})
	for conn := range h.connections.m {
		//For every connected user, do something with the message or disconnect
		//Each user may have a different delay, but no user blocks others

		//To simulate different users getting different messages, we'll send timestamps and sleep, too:
		log.Printf("hub.bcast: conn.Send'ing message '''%v''' to conn %v\n", string(message), conn)

		//Do not wait for one client's send before launching the next
		go conn.Send(message, finChan, h)
		i++
	}

	//Done iterating over the map
	h.connections.mu.RUnlock()

	//Drain all finChan values; afterwards, we'll unblock
	for i > 0 {
		select {
		case <-finChan:
			i--
		}
	}

	log.Printf("hub.bcast: bcast'ing message ```%v``` is done.", string(message))
}
