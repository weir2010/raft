package raft

import (
	"errors"
	"sync"
	"time"
	"fmt"
)

//------------------------------------------------------------------------------
//
// Typedefs
//
//------------------------------------------------------------------------------

// A peer is a reference to another server involved in the consensus protocol.
type Peer struct {
	server         *Server
	name           string
	prevLogIndex   uint64
	mutex          sync.Mutex
	heartbeatTimer *Timer
}

//------------------------------------------------------------------------------
//
// Constructor
//
//------------------------------------------------------------------------------

// Creates a new peer.
func NewPeer(server *Server, name string, heartbeatTimeout time.Duration) *Peer {
	p := &Peer{
		server:         server,
		name:           name,
		heartbeatTimer: NewTimer(heartbeatTimeout, heartbeatTimeout),
	}

	// Start the heartbeat timeout and wait for the goroutine to start.
	c := make(chan bool)
	go p.heartbeatTimeoutFunc(c)
	<-c
	
	return p
}

//------------------------------------------------------------------------------
//
// Accessors
//
//------------------------------------------------------------------------------

// Retrieves the name of the peer.
func (p *Peer) Name() string {
	return p.name
}

// Retrieves the heartbeat timeout.
func (p *Peer) HeartbeatTimeout() time.Duration {
	return p.heartbeatTimer.MinDuration()
}

// Sets the heartbeat timeout.
func (p *Peer) SetHeartbeatTimeout(duration time.Duration) {
	p.heartbeatTimer.SetDuration(duration)
}

//------------------------------------------------------------------------------
//
// Methods
//
//------------------------------------------------------------------------------

//--------------------------------------
// State
//--------------------------------------

// Resumes the peer heartbeating.
func (p *Peer) resume() {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.heartbeatTimer.Reset()
}

// Pauses the peer to prevent heartbeating.
func (p *Peer) pause() {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.heartbeatTimer.Pause()
}

// Stops the peer entirely.
func (p *Peer) stop() {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.heartbeatTimer.Stop()
}

//--------------------------------------
// Flush
//--------------------------------------

// Sends an AppendEntries RPC but does not obtain a lock on the server. This
// method should only be called from the server.
func (p *Peer) internalFlush() (uint64, bool, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	fmt.Println("internal flush!")
	if p.prevLogIndex < p.server.log.StartIndex() {
		req := p.server.createSnapshotRequest()
		return p.sendSnapshotRequest(req)
	}
	req := p.server.createInternalAppendEntriesRequest(p.prevLogIndex)
	return p.sendFlushRequest(req)
}

// TODO add this function
func (p *Peer) sendSnapshotRequest(req *SnapshotRequest) (uint64, bool, error){
	// Ignore any null requests.
	if req == nil {
		return 0, false, errors.New("raft.Peer: Request required")
	}

	// Generate an snapshot request based on the state of the server and
	// log. Send the request through the user-provided handler and process the
	// result.
	resp, err := p.server.transporter.SendSnapshotRequest(p.server, p, req)
	p.heartbeatTimer.Reset()
	if resp == nil {
		return 0, false, err
	}

	// If successful then update the previous log index. If it was
	// unsuccessful then decrement the previous log index and we'll try again
	// next time.
	if resp.Success {
		p.prevLogIndex = req.LastIndex
		fmt.Println("update peer preindex to ", p.prevLogIndex)
	} else {
		panic(resp)
	}

	return resp.Term, resp.Success, err	
}

// Flushes a request through the server's transport.
func (p *Peer) sendFlushRequest(req *AppendEntriesRequest) (uint64, bool, error) {
	// Ignore any null requests.
	if req == nil {
		return 0, false, errors.New("raft.Peer: Request required")
	}
	fmt.Println("FLUSH: before trans!")
	// Generate an AppendEntries request based on the state of the server and
	// log. Send the request through the user-provided handler and process the
	// result.
	resp, err := p.server.transporter.SendAppendEntriesRequest(p.server, p, req)
	fmt.Println("FLUSH: trans finished")
	p.heartbeatTimer.Reset()
	if resp == nil {
		fmt.Println("trans error")
		return 0, false, err
	}

	// If successful then update the previous log index. If it was
	// unsuccessful then decrement the previous log index and we'll try again
	// next time.
	if resp.Success {
		fmt.Println("FLUSH: trans success")
		if len(req.Entries) > 0 {
			p.prevLogIndex = req.Entries[len(req.Entries)-1].Index
		}
	} else {
		// Decrement the previous log index down until we find a match. Don't
		// let it go below where the peer's commit index is though. That's a
		// problem.
		if p.prevLogIndex > 0 {
			p.prevLogIndex--
		}
		if resp.CommitIndex > p.prevLogIndex {
			p.prevLogIndex = resp.CommitIndex
		}
	}

	return resp.Term, resp.Success, err
}

//--------------------------------------
// Heartbeat
//--------------------------------------

// Listens to the heartbeat timeout and flushes an AppendEntries RPC.
func (p *Peer) heartbeatTimeoutFunc(startChannel chan bool) {
	startChannel <- true

	for {
		// Grab the current timer channel.
		p.mutex.Lock()
		fmt.Println("heart beat: got lock")
		var c chan time.Time
		if p.heartbeatTimer != nil {
			c = p.heartbeatTimer.C()
		}
		p.mutex.Unlock()
		fmt.Println("heart beat: after lock")
		// If the channel or timer are gone then exit.
		if c == nil {
			fmt.Println("heart beat: break")
			break
		}

		// Flush the peer when we get a heartbeat timeout. If the channel is
		// closed then the peer is getting cleaned up and we should exit.
		if _, ok := <-c; ok {
			// Retrieve the peer data within a lock that is separate from the
			// server lock when creating the request. Otherwise a deadlock can
			// occur.
			p.mutex.Lock()
			server, prevLogIndex := p.server, p.prevLogIndex
			p.mutex.Unlock()
			
			fmt.Println("heart beat, preIndex: ", prevLogIndex, " startIndex:", server.log.StartIndex())
			
			server.log.mutex.Lock()
			if prevLogIndex < server.log.StartIndex() {
				server.log.mutex.Unlock()
				req := server.createSnapshotRequest()

				p.mutex.Lock()
				p.sendSnapshotRequest(req)
				p.mutex.Unlock()
			} else {

				// Lock the server to create a request.
				req := server.createAppendEntriesRequest(prevLogIndex)
				server.log.mutex.Unlock()
				p.mutex.Lock()
				p.sendFlushRequest(req)
				p.mutex.Unlock()
			}
		} else {
			break
		}
	}
}
