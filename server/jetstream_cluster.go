// Copyright 2020-2021 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/s2"
	"github.com/nats-io/nuid"
)

// jetStreamCluster holds information about the meta group and stream assignments.
type jetStreamCluster struct {
	// The metacontroller raftNode.
	meta RaftNode
	// For stream and consumer assignments. All servers will have this be the same.
	// ACC -> STREAM -> Stream Assignment -> Consumers
	streams map[string]map[string]*streamAssignment
	// Server
	s *Server
}

// Define types of the entry.
type entryOp uint8

const (
	// Meta ops.
	assignStreamOp entryOp = iota
	assignConsumerOp
	removeStreamOp
	removeConsumerOp
	// Stream ops.
	streamMsgOp
	purgeStreamOp
	deleteMsgOp
	// Consumer ops
	updateDeliveredOp
	updateAcksOp
	updateFullStateOp
)

// raftGroup are controlled by the metagroup controller. The raftGroups will
// house streams and consumers.
type raftGroup struct {
	Name      string      `json:"name"`
	Peers     []string    `json:"peers"`
	Storage   StorageType `json:"store"`
	Preferred string      `json:"preferred,omitempty"`
	// Internal
	node RaftNode
}

// streamAssignment is what the meta controller uses to assign streams to peers.
type streamAssignment struct {
	Client *ClientInfo   `json:"client,omitempty"`
	Config *StreamConfig `json:"stream"`
	Group  *raftGroup    `json:"group"`
	Sync   string        `json:"sync"`
	Reply  string        `json:"reply"`
	// Internal
	consumers map[string]*consumerAssignment
	responded bool
	err       error
}

// streamPurge is what the stream leader will replicate when purging a stream.
type streamPurge struct {
	Client *ClientInfo `json:"client,omitempty"`
	Stream string      `json:"stream"`
	Reply  string      `json:"reply"`
	// Internal
	responded bool
	err       error
}

// streamMsgDelete is what the stream leader will replicate when deleting a message.
type streamMsgDelete struct {
	Client *ClientInfo `json:"client,omitempty"`
	Stream string      `json:"stream"`
	Seq    uint64      `json:"seq"`
	Reply  string      `json:"reply"`
	// Internal
	responded bool
	err       error
}

// consumerAssignment is what the meta controller uses to assign consumers to streams.
type consumerAssignment struct {
	Client *ClientInfo     `json:"client,omitempty"`
	Name   string          `json:"name"`
	Stream string          `json:"stream"`
	Config *ConsumerConfig `json:"consumer"`
	Group  *raftGroup      `json:"group"`
	Reply  string          `json:"reply"`
	// Internal
	responded bool
	err       error
}

const (
	defaultStoreDirName  = "_js_"
	defaultMetaGroupName = "_meta_"
	defaultMetaFSBlkSize = 64 * 1024
)

// For validating clusters.
func validateJetStreamOptions(o *Options) error {
	// If not clustered no checks.
	if !o.JetStream || o.Cluster.Port == 0 {
		return nil
	}
	if o.ServerName == _EMPTY_ {
		return fmt.Errorf("jetstream cluster requires `server_name` to be set")
	}
	if o.Cluster.Name == _EMPTY_ {
		return fmt.Errorf("jetstream cluster requires `cluster_name` to be set")
	}
	return nil
}

func (s *Server) getJetStreamCluster() (*jetStream, *jetStreamCluster) {
	s.mu.Lock()
	shutdown := s.shutdown
	js := s.js
	s.mu.Unlock()

	if shutdown || js == nil {
		return nil, nil
	}

	js.mu.RLock()
	cc := js.cluster
	js.mu.RUnlock()
	if cc == nil {
		return nil, nil
	}
	return js, cc
}

func (s *Server) JetStreamIsClustered() bool {
	js := s.getJetStream()
	if js == nil {
		return false
	}
	js.mu.RLock()
	isClustered := js.cluster != nil
	js.mu.RUnlock()
	return isClustered
}

func (s *Server) JetStreamIsLeader() bool {
	js := s.getJetStream()
	if js == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.cluster.isLeader()
}

func (s *Server) JetStreamIsCurrent() bool {
	js := s.getJetStream()
	if js == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.cluster.isCurrent()
}

func (s *Server) JetStreamSnapshotMeta() error {
	js := s.getJetStream()
	if js == nil {
		return ErrJetStreamNotEnabled
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	cc := js.cluster
	if !cc.isLeader() {
		return errNotLeader
	}
	return cc.meta.Snapshot(js.metaSnapshot())
}

func (s *Server) JetStreamSnapshotStream(account, stream string) error {
	js, cc := s.getJetStreamCluster()
	if js == nil {
		return ErrJetStreamNotEnabled
	}
	if cc == nil {
		return ErrJetStreamNotClustered
	}
	// Grab account
	acc, err := s.LookupAccount(account)
	if err != nil {
		return err
	}
	// Grab stream
	mset, err := acc.LookupStream(stream)
	if err != nil {
		return err
	}

	mset.mu.RLock()
	if !mset.node.Leader() {
		mset.mu.RUnlock()
		return ErrJetStreamNotLeader
	}
	state := mset.store.State()
	b, _ := json.Marshal(state)
	fmt.Printf("\n\n[%s] - Stream state encoded is '%s'\n\n", s, b)
	mset.node.Snapshot(b)
	mset.mu.RUnlock()

	return nil
}

func (s *Server) JetStreamClusterPeers() []string {
	js := s.getJetStream()
	if js == nil {
		return nil
	}
	js.mu.RLock()
	defer js.mu.RUnlock()

	cc := js.cluster
	if !cc.isLeader() {
		return nil
	}
	peers := cc.meta.Peers()
	var nodes []string
	for _, p := range peers {
		nodes = append(nodes, p.ID)
	}
	return nodes
}

// Read lock should be held.
func (cc *jetStreamCluster) isLeader() bool {
	if cc == nil {
		// Non-clustered mode
		return true
	}
	return cc.meta.Leader()
}

// isCurrent will determine if this node is a leader or an up to date follower.
// Read lock should be held.
func (cc *jetStreamCluster) isCurrent() bool {
	if cc == nil {
		// Non-clustered mode
		return true
	}
	return cc.meta.Current()
}

// isStreamCurrent will determine if this node is a participant for the stream and if its up to date.
// Read lock should be held.
func (cc *jetStreamCluster) isStreamCurrent(account, stream string) bool {
	if cc == nil {
		// Non-clustered mode
		return true
	}
	as := cc.streams[account]
	if as == nil {
		return false
	}
	sa := as[stream]
	if sa == nil {
		return false
	}
	rg := sa.Group
	if rg == nil || rg.node == nil {
		return false
	}
	return rg.node.Current()
}

func (a *Account) getJetStreamFromAccount() (*Server, *jetStream, *jsAccount) {
	a.mu.RLock()
	jsa := a.js
	a.mu.RUnlock()
	if jsa == nil {
		return nil, nil, nil
	}
	jsa.mu.RLock()
	js := jsa.js
	jsa.mu.RUnlock()
	if js == nil {
		return nil, nil, nil
	}
	js.mu.RLock()
	s := js.srv
	js.mu.RUnlock()
	return s, js, jsa
}

// jetStreamReadAllowedForStream will check if we can report on any information
// regarding this stream like info, etc.
func (a *Account) jetStreamReadAllowedForStream(stream string) bool {
	s, js, jsa := a.getJetStreamFromAccount()
	if s == nil || js == nil || jsa == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	// TODO(dlc) - make sure we are up to date.
	return js.cluster.isStreamAssigned(a, stream)
}

func (s *Server) JetStreamIsStreamLeader(account, stream string) bool {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return false
	}
	return cc.isStreamLeader(account, stream)
}

func (a *Account) JetStreamIsStreamLeader(stream string) bool {
	s, js, jsa := a.getJetStreamFromAccount()
	if s == nil || js == nil || jsa == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.cluster.isStreamLeader(a.Name, stream)
}

func (s *Server) JetStreamIsStreamCurrent(account, stream string) bool {
	js, cc := s.getJetStreamCluster()
	if js == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return cc.isStreamCurrent(account, stream)
}

func (a *Account) JetStreamIsConsumerLeader(stream, consumer string) bool {
	s, js, jsa := a.getJetStreamFromAccount()
	if s == nil || js == nil || jsa == nil {
		return false
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.cluster.isConsumerLeader(a.Name, stream, consumer)
}

func (s *Server) JetStreamIsConsumerLeader(account, stream, consumer string) bool {
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return false
	}
	return cc.isConsumerLeader(account, stream, consumer)
}

func (s *Server) jetStreamReadAllowed() bool {
	// FIXME(dlc) - Add in read allowed mode for readonly API.
	return s.JetStreamIsLeader()
}

func (s *Server) enableJetStreamClustering() error {
	if !s.isRunning() {
		return nil
	}
	js := s.getJetStream()
	if js == nil {
		return ErrJetStreamNotEnabled
	}
	// Already set.
	if js.cluster != nil {
		return nil
	}

	s.Noticef("Starting JetStream cluster")
	// We need to determine if we have a stable cluster name and expected number of servers.
	s.Debugf("JetStream cluster checking for stable cluster name and peers")
	if s.isClusterNameDynamic() || s.configuredRoutes() == 0 {
		return errors.New("JetStream cluster requires cluster name and explicit routes")
	}

	return js.setupMetaGroup()
}

func (js *jetStream) setupMetaGroup() error {
	s := js.srv
	fmt.Printf("creating metagroup!\n")
	fmt.Printf("cluster name is stable, numConfiguredRoutes is %d\n", s.configuredRoutes())

	// Setup our WAL for the metagroup.
	stateDir := path.Join(js.config.StoreDir, defaultStoreDirName, defaultMetaGroupName)
	fs, bootstrap, err := newFileStore(
		FileStoreConfig{StoreDir: stateDir, BlockSize: defaultMetaFSBlkSize},
		StreamConfig{Name: defaultMetaGroupName, Storage: FileStorage},
	)
	if err != nil {
		fmt.Printf("got err! %v\n", err)
		return err
	}

	cfg := &RaftConfig{Name: defaultMetaGroupName, Store: stateDir, Log: fs}

	if bootstrap {
		s.Noticef("JetStream cluster bootstrapping")
		// FIXME(dlc) - Make this real.
		peers := s.activePeers()
		s.Debugf("JetStream cluster initial peers: %+v", peers)
		fmt.Printf("JetStream cluster initial peers: %+v\n", peers)
		s.bootstrapRaftNode(cfg, peers, false)
	} else {
		fmt.Printf("[%s] Recovering state from %q\n", s, stateDir)
		s.Noticef("JetStream cluster recovering state")
	}
	// Start up our meta node.
	n, err := s.startRaftNode(cfg)
	if err != nil {
		fmt.Printf("\nCould not start RAFT!! %v\n\n", err)
		return err
	}

	js.mu.Lock()
	defer js.mu.Unlock()
	js.cluster = &jetStreamCluster{
		meta:    n,
		streams: make(map[string]map[string]*streamAssignment),
		s:       s,
	}

	js.srv.startGoRoutine(js.monitorCluster)
	return nil
}

func (js *jetStream) getMetaGroup() RaftNode {
	js.mu.RLock()
	defer js.mu.RUnlock()
	if js.cluster == nil {
		return nil
	}
	return js.cluster.meta
}

func (js *jetStream) server() *Server {
	js.mu.RLock()
	s := js.srv
	js.mu.RUnlock()
	return s
}

// streamAssigned informs us if this server has this stream assigned.
func (jsa *jsAccount) streamAssigned(stream string) bool {
	jsa.mu.RLock()
	defer jsa.mu.RUnlock()

	js, acc := jsa.js, jsa.account
	if js == nil {
		return false
	}
	return js.cluster.isStreamAssigned(acc, stream)
}

// Read lock should be held.
func (cc *jetStreamCluster) isStreamAssigned(a *Account, stream string) bool {
	// Non-clustered mode always return true.
	if cc == nil {
		return true
	}
	fmt.Printf("[%s] - Checking cc.streams of %+v\n", a.srv.Name(), cc.streams)
	as := cc.streams[a.Name]
	if as == nil {
		return false
	}
	sa := as[stream]
	if sa == nil {
		return false
	}
	rg := sa.Group
	if rg == nil {
		return false
	}
	// Check if we are the leader of this raftGroup assigned to the stream.
	ourID := cc.meta.ID()
	for _, peer := range rg.Peers {
		if peer == ourID {
			return true
		}
	}
	return false
}

// Read lock should be held.
func (cc *jetStreamCluster) isStreamLeader(account, stream string) bool {
	// Non-clustered mode always return true.
	if cc == nil {
		return true
	}
	var sa *streamAssignment
	if as := cc.streams[account]; as != nil {
		sa = as[stream]
	}
	if sa == nil {
		return false
	}
	rg := sa.Group
	if rg == nil {
		return false
	}
	// Check if we are the leader of this raftGroup assigned to the stream.
	ourID := cc.meta.ID()
	for _, peer := range rg.Peers {
		if peer == ourID {
			if len(rg.Peers) == 1 || rg.node.Leader() {
				return true
			}
		}
	}
	return false
}

// Read lock should be held.
func (cc *jetStreamCluster) isConsumerLeader(account, stream, consumer string) bool {
	// Non-clustered mode always return true.
	if cc == nil {
		return true
	}
	var sa *streamAssignment
	if as := cc.streams[account]; as != nil {
		sa = as[stream]
	}
	if sa == nil {
		return false
	}
	// Check if we are the leader of this raftGroup assigned to this consumer.
	ourID := cc.meta.ID()
	for _, ca := range sa.consumers {
		rg := ca.Group
		for _, peer := range rg.Peers {
			if peer == ourID {
				if len(rg.Peers) == 1 || rg.node.Leader() {
					return true
				}
			}
		}
	}
	return false
}

func (js *jetStream) monitorCluster() {
	fmt.Printf("[%s] Starting monitor cluster routine\n", js.srv)
	defer fmt.Printf("[%s] Exiting monitor cluster routine\n", js.srv)

	s, n := js.server(), js.getMetaGroup()
	qch, lch, ach := n.QuitC(), n.LeadChangeC(), n.ApplyC()

	defer s.grWG.Done()

	for {
		select {
		case <-s.quitCh:
			return
		case <-qch:
			return
		case ce := <-ach:
			// FIXME(dlc) - Deal with errors.
			js.applyMetaEntries(ce.Entries)
			//js.writeMetaState(ce.Index)
			n.Applied(ce.Index)
		case isLeader := <-lch:
			js.processLeaderChange(isLeader)
		}
	}
}

// Represents our stable meta state that we can write out.
type writeableStreamAssignment struct {
	Client    *ClientInfo   `json:"client,omitempty"`
	Config    *StreamConfig `json:"stream"`
	Group     *raftGroup    `json:"group"`
	Sync      string        `json:"sync"`
	Consumers []*consumerAssignment
}

func (js *jetStream) metaSnapshot() []byte {
	var streams []writeableStreamAssignment
	js.mu.RLock()
	cc := js.cluster
	for _, asa := range cc.streams {
		for _, sa := range asa {
			wsa := writeableStreamAssignment{
				Client: sa.Client,
				Config: sa.Config,
				Group:  sa.Group,
				Sync:   sa.Sync,
			}
			for _, ca := range sa.consumers {
				wsa.Consumers = append(wsa.Consumers, ca)
			}
			streams = append(streams, wsa)
		}
	}
	js.mu.RUnlock()

	if len(streams) == 0 {
		return nil
	}

	b, _ := json.Marshal(streams)
	return s2.EncodeBetter(nil, b)
}

func (js *jetStream) applyMetaSnapshot(buf []byte) error {
	jse, err := s2.Decode(nil, buf)
	if err != nil {
		return err
	}
	var wsas []writeableStreamAssignment
	if err = json.Unmarshal(jse, &wsas); err != nil {
		return err
	}
	fmt.Printf("[%s] Got snapshot %+v\n", js.srv, wsas)
	// Build our new version here outside of js.
	streams := make(map[string]map[string]*streamAssignment)
	for _, wsa := range wsas {
		as := streams[wsa.Client.Account]
		if as == nil {
			as = make(map[string]*streamAssignment)
			streams[wsa.Client.Account] = as
		}
		sa := &streamAssignment{Client: wsa.Client, Config: wsa.Config, Group: wsa.Group, Sync: wsa.Sync}
		if len(wsa.Consumers) > 0 {
			sa.consumers = make(map[string]*consumerAssignment)
			for _, ca := range wsa.Consumers {
				sa.consumers[ca.Name] = ca
			}
		}
		as[wsa.Config.Name] = sa
	}

	js.mu.Lock()
	cc := js.cluster

	fmt.Printf("Generated clone: %+v\n", streams)
	fmt.Printf("Original: %+v\n", cc.streams)

	var saAdd, saDel, saChk []*streamAssignment
	// Walk through the old list to generate the delete list.
	for account, asa := range cc.streams {
		nasa := streams[account]
		for sn, sa := range asa {
			if nsa := nasa[sn]; nsa == nil {
				saDel = append(saDel, sa)
				fmt.Printf("[%s] NEED TO REMOVE SA %+v\n", js.srv, sa)
			} else {
				saChk = append(saChk, nsa)
				fmt.Printf("[%s] NEED TO CHECK SA %+v\n", js.srv, nsa)
			}
		}
	}
	// Walk through the new list to generate the add list.
	for account, nasa := range streams {
		asa := cc.streams[account]
		for sn, sa := range nasa {
			if asa[sn] == nil {
				saAdd = append(saAdd, sa)
				fmt.Printf("[%s] NEED TO ADD SA %+v\n", js.srv, sa)
			}
		}
	}
	// Now walk the ones to check and process consumers.
	var caAdd, caDel []*consumerAssignment
	for _, sa := range saChk {
		if osa := js.streamAssignment(sa.Client.Account, sa.Config.Name); osa != nil {
			for _, ca := range osa.consumers {
				if sa.consumers[ca.Name] == nil {
					fmt.Printf("[%s] NEED TO REMOVE CA %+v\n", js.srv, ca)
					caDel = append(caDel, ca)
				} else {
					fmt.Printf("[%s] NEED TO [MAYBE] ADD CA %+v\n", js.srv, ca)
					caAdd = append(caAdd, ca)
				}
			}
		}
	}
	js.mu.Unlock()

	// Do removals first.
	for _, sa := range saDel {
		js.processStreamRemoval(sa)
	}
	// Now do add for the streams. Also add in all consumers.
	for _, sa := range saAdd {
		js.processStreamAssignment(sa)
		// We can simply add the consumers.
		for _, ca := range sa.consumers {
			js.processConsumerAssignment(ca)
		}
	}
	// Now do the deltas for existing stream's consumers.
	for _, ca := range caDel {
		js.processConsumerRemoval(ca)
	}
	for _, ca := range caAdd {
		js.processConsumerAssignment(ca)
	}

	return nil
}

// FIXME(dlc) - Return error. Don't apply above if err.
func (js *jetStream) applyMetaEntries(entries []*Entry) {
	fmt.Printf("[%s] JS HAS AN ENTRIES UPDATE TO APPLY!\n", js.srv)

	for _, e := range entries {
		if e.Type == EntrySnapshot {
			fmt.Printf("[%s] SNAPSHOT META ENTRY\n", js.srv)
			js.applyMetaSnapshot(e.Data)
		} else {
			buf := e.Data
			switch entryOp(buf[0]) {
			case assignStreamOp:
				fmt.Printf("[%s] STREAM ASSIGN ENTRY\n", js.srv)
				sa, err := decodeStreamAssignment(buf[1:])
				if err != nil {
					js.srv.Errorf("JetStream cluster failed to decode stream assignment: %q", buf[1:])
					return
				}
				js.processStreamAssignment(sa)
			case removeStreamOp:
				fmt.Printf("[%s] REMOVE STREAM ENTRY\n", js.srv)
				sa, err := decodeStreamAssignment(buf[1:])
				if err != nil {
					js.srv.Errorf("JetStream cluster failed to decode stream assignment: %q", buf[1:])
					return
				}
				js.processStreamRemoval(sa)
			case assignConsumerOp:
				fmt.Printf("[%s] CONSUMER ASSIGN ENTRY\n", js.srv)
				ca, err := decodeConsumerAssignment(buf[1:])
				if err != nil {
					js.srv.Errorf("JetStream cluster failed to decode consumer assigment: %q", buf[1:])
					return
				}
				js.processConsumerAssignment(ca)
			case removeConsumerOp:
				fmt.Printf("[%s] CONSUMER REMOVE ENTRY\n", js.srv)
				ca, err := decodeConsumerAssignment(buf[1:])
				if err != nil {
					js.srv.Errorf("JetStream cluster failed to decode consumer assigment: %q", buf[1:])
					return
				}
				js.processConsumerRemoval(ca)
			default:
				panic("JetStream Cluster Unknown meta entry op type!")
			}
		}
	}
}

// Lock should be held.
func (js *jetStream) nodeID() string {
	if js.cluster == nil {
		return _EMPTY_
	}
	return js.cluster.meta.ID()
}

func (rg *raftGroup) isMember(id string) bool {
	if rg == nil {
		return false
	}
	for _, peer := range rg.Peers {
		if peer == id {
			return true
		}
	}
	return false
}

// createRaftGroup is called to spin up this raft group if needed.
func (js *jetStream) createRaftGroup(rg *raftGroup) {
	js.mu.Lock()
	defer js.mu.Unlock()

	s, cc := js.srv, js.cluster

	// If this is a single peer raft group or we are not a member return.
	if len(rg.Peers) <= 1 || !rg.isMember(cc.meta.ID()) {
		// Nothing to do here.
		return
	}

	// We already have this assigned.
	if node := s.lookupRaftNode(rg.Name); node != nil {
		s.Debugf("JetStream cluster already has raft group %q assigned", rg.Name)
		return
	}

	s.Debugf("JetStream cluster creating raft group:%+v", rg)
	fmt.Printf("[%s:%s]\tJetStream cluster assigning raft group:%+v\n", s.Name(), js.nodeID(), rg)

	sysAcc := s.SystemAccount()
	if sysAcc == nil {
		s.Debugf("JetStream cluster detected shutdown processing raft group:%+v", rg)
		fmt.Printf("[%s] JetStream cluster detected shutdown processing raft group:%+v\n", s.Name(), rg)
		return
	}

	stateDir := path.Join(js.config.StoreDir, sysAcc.Name, defaultStoreDirName, rg.Name)
	fs, bootstrap, err := newFileStore(
		FileStoreConfig{StoreDir: stateDir},
		StreamConfig{Name: rg.Name, Storage: rg.Storage},
	)
	if err != nil {
		fmt.Printf("got err! %v\n", err)
		return
	}
	fmt.Printf("[%s] Will create raft group %q for %q\n", s.Name(), rg.Name, stateDir)

	cfg := &RaftConfig{Name: rg.Name, Store: stateDir, Log: fs}

	if bootstrap {
		s.bootstrapRaftNode(cfg, rg.Peers, true)
	}
	n, err := s.startRaftNode(cfg)
	if err != nil {
		fmt.Printf("ERROR CREATING RAFT GROUP!!!%v\n", err)
		return
	}
	rg.node = n
	fmt.Printf("[%s] Created group %q\n", s.Name(), rg.Name)
}

func (mset *Stream) raftNode() RaftNode {
	if mset == nil {
		return nil
	}
	mset.mu.RLock()
	defer mset.mu.RUnlock()
	return mset.node
}

func (js *jetStream) monitorStreamRaftGroup(mset *Stream, sa *streamAssignment) {
	fmt.Printf("[%s:%s] Starting stream monitor raft group routine\n", js.srv.Name(), sa.Group.Name)
	defer fmt.Printf("[%s:%s] Exiting stream monitor raft group routine\n", js.srv.Name(), sa.Group.Name)

	s, n := js.server(), mset.raftNode()
	if n == nil {
		s.Warnf("JetStream cluster can't monitor stream raft group, account %q, stream %q", sa.Client.Account, sa.Config.Name)
		return
	}
	qch, lch, ach := n.QuitC(), n.LeadChangeC(), n.ApplyC()

	defer s.grWG.Done()

	for {
		select {
		case <-s.quitCh:
			return
		case <-qch:
			return
		case ce := <-ach:
			// FIXME(dlc) - capture errors.
			js.applyStreamEntries(mset, ce)
			n.Applied(ce.Index)
		case isLeader := <-lch:
			js.processStreamLeaderChange(mset, sa, isLeader)
		}
	}
}

func (js *jetStream) applyStreamEntries(mset *Stream, ce *CommittedEntry) {
	fmt.Printf("[%s] JS GROUP %q HAS STREAM ENTRIES UPDATE TO APPLY!\n", js.srv, mset.node.Group())
	for _, e := range ce.Entries {
		if e.Type == EntrySnapshot {
			mset.processSnapshot(e.Data)
		} else {
			buf := e.Data
			switch entryOp(buf[0]) {
			case streamMsgOp:
				subject, reply, hdr, msg, lseq, ts, err := decodeStreamMsg(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				fmt.Printf("[%s] DECODED %q %q %q %q\n\n", js.srv.Name(), subject, reply, hdr, msg)
				// processJetStreamMsg will respond to the client below if we are the leader.
				if err := mset.processJetStreamMsg(subject, reply, hdr, msg, lseq, ts); err != nil {
					if err == errLastSeqMismatch {
						// TODO(dlc) - Should we care here if this is < LastSeq vs not?
						fmt.Printf("[%s] Ignoring message with expected seq of %d\n", js.srv, lseq+1)
					} else {
						panic(err.Error())
					}
				}
			case deleteMsgOp:
				//fmt.Printf("\n\n[%s] MSG DELETE DECODED CALLED\n\n", js.srv)
				md, err := decodeMsgDelete(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				s, cc := js.server(), js.cluster
				fmt.Printf("[%s] MSG DELETE DECODED %+v\n\n", s, md)
				removed, err := mset.EraseMsg(md.Seq)
				if err != nil {
					s.Warnf("JetStream cluster failed to delete msg %d from stream %q for account %q: %v", md.Seq, md.Stream, md.Client.Account, err)
				}
				js.mu.RLock()
				isLeader := cc.isStreamLeader(md.Client.Account, md.Stream)
				js.mu.RUnlock()
				if isLeader {
					fmt.Printf("[%s] MSG DELETE of %d, SHOULD RESPOND AS LEADER to %q\n\n", s, md.Seq, md.Reply)
					var resp = JSApiMsgDeleteResponse{ApiResponse: ApiResponse{Type: JSApiMsgDeleteResponseType}}
					if err != nil {
						resp.Error = jsError(err)
					} else if !removed {
						resp.Error = &ApiError{Code: 400, Description: fmt.Sprintf("sequence [%d] not found", md.Seq)}
					} else {
						resp.Success = true
					}
					s.sendAPIResponse(md.Client, mset.account(), _EMPTY_, md.Reply, _EMPTY_, s.jsonResponse(resp))
				}
			case purgeStreamOp:
				sp, err := decodeStreamPurge(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				s := js.server()
				fmt.Printf("[%s] PURGE DECODED %+v\n\n", s, sp)
				purged, err := mset.Purge()
				if err != nil {
					s.Warnf("JetStream cluster failed to purge stream %q for account %q: %v", sp.Stream, sp.Client.Account, err)
				}
				js.mu.RLock()
				isLeader := js.cluster.isStreamLeader(sp.Client.Account, sp.Stream)
				js.mu.RUnlock()
				if isLeader {
					fmt.Printf("[%s] PURGED %d, SHOULD RESPOND AS LEADER\n\n", s, purged)
					var resp = JSApiStreamPurgeResponse{ApiResponse: ApiResponse{Type: JSApiStreamPurgeResponseType}}
					if err != nil {
						resp.Error = jsError(err)
					} else {
						resp.Purged = purged
						resp.Success = true
					}
					s.sendAPIResponse(sp.Client, mset.account(), _EMPTY_, sp.Reply, _EMPTY_, s.jsonResponse(resp))
				}
			default:
				panic("JetStream Cluster Unknown group entry op type!")
			}
		}
	}
}

func (js *jetStream) processStreamLeaderChange(mset *Stream, sa *streamAssignment, isLeader bool) {
	fmt.Printf("\n\n[%s] JS detected stream leadership change for %q! %v\n", js.srv.Name(), sa.Group.Name, isLeader)

	mset.setLeader(isLeader)

	if !isLeader {
		return
	}

	// Check if we need to respond to the original request.
	// FIXME(dlc) - This approach does not do what we really want. Needs to be fixed.
	js.mu.Lock()
	responded, err := sa.responded, sa.err
	sa.responded = true
	s := js.srv
	js.mu.Unlock()

	if !responded {
		acc := mset.jsa.acc()
		var resp = JSApiStreamCreateResponse{ApiResponse: ApiResponse{Type: JSApiStreamCreateResponseType}}
		if err != nil {
			resp.Error = jsError(err)
			s.sendAPIResponse(sa.Client, acc, _EMPTY_, sa.Reply, _EMPTY_, s.jsonResponse(&resp))
		} else {
			fmt.Printf("\n\n[%s] - Successfully created our stream!!! %+v\n\n", s.Name(), mset)
			resp.StreamInfo = &StreamInfo{Created: mset.Created(), State: mset.State(), Config: mset.Config()}
			js.srv.sendAPIResponse(sa.Client, acc, _EMPTY_, sa.Reply, _EMPTY_, s.jsonResponse(&resp))
		}
	}
}

// Will lookup a stream assignment.
// Lock should be held.
func (js *jetStream) streamAssignment(account, stream string) (sa *streamAssignment) {
	if as := js.cluster.streams[account]; as != nil {
		sa = as[stream]
	}
	return sa
}

// processStreamAssignment is called when followers have replicated an assignment.
func (js *jetStream) processStreamAssignment(sa *streamAssignment) {
	fmt.Printf("[%s] Got a stream assignment %+v\n", js.srv.Name(), sa)

	js.mu.RLock()
	s, cc := js.srv, js.cluster
	js.mu.RUnlock()
	if s == nil || cc == nil {
		// TODO(dlc) - debug at least
		return
	}

	acc, err := s.LookupAccount(sa.Client.Account)
	if err != nil {
		// TODO(dlc) - log error
		return
	}
	stream := sa.Config.Name

	js.mu.Lock()
	// Check if we already have this assigned.
	accStreams := cc.streams[acc.Name]
	if accStreams != nil && accStreams[stream] != nil {
		// TODO(dlc) - Debug?
		// We already have this assignment, should we check they are the same?
		js.mu.Unlock()
		return
	}
	if accStreams == nil {
		accStreams = make(map[string]*streamAssignment)
	}
	// Update our state.
	accStreams[stream] = sa
	cc.streams[acc.Name] = accStreams

	fmt.Printf("Assigned %+v\n", cc.streams)
	isMember := sa.Group.isMember(cc.meta.ID())
	js.mu.Unlock()

	// Check if this is for us..
	if isMember {
		fmt.Printf("Will process since we are a member!!\n")
		js.processClusterCreateStream(sa)
	}
}

// processStreamRemoval is called when followers have replicated an assignment.
func (js *jetStream) processStreamRemoval(sa *streamAssignment) {
	fmt.Printf("[%s] Got a stream removal %+v\n", js.srv.Name(), sa)

	js.mu.RLock()
	s, cc := js.srv, js.cluster
	js.mu.RUnlock()
	if s == nil || cc == nil {
		// TODO(dlc) - debug at least
		return
	}
	stream := sa.Config.Name

	js.mu.Lock()

	wasLeader := cc.isStreamLeader(sa.Client.Account, stream)

	// Check if we already have this assigned.
	accStreams := cc.streams[sa.Client.Account]
	needDelete := accStreams != nil && accStreams[stream] != nil
	if needDelete {
		delete(accStreams, stream)
		if len(accStreams) == 0 {
			delete(cc.streams, sa.Client.Account)
		}
	}
	js.mu.Unlock()

	if !needDelete {
		return
	}

	fmt.Printf("Will process remove stream regardless of membership!!\n")
	js.processClusterDeleteStream(sa, wasLeader)
}

// processClusterCreateStream is called when we have a stream assignment that
// has been committed and this server is a member of the peer group.
func (js *jetStream) processClusterCreateStream(sa *streamAssignment) {
	if sa == nil {
		return
	}
	fmt.Printf("[%s] Stream Assignment Recording\n", js.srv.Name())
	fmt.Printf("sa.Config is %+v\n", sa.Config)

	js.mu.RLock()
	s := js.srv
	acc, err := s.LookupAccount(sa.Client.Account)
	if err != nil {
		s.Warnf("JetStream cluster failed to lookup account %q: %v", sa.Client.Account, err)
		js.mu.RUnlock()
		return
	}
	rg := sa.Group
	js.mu.RUnlock()

	// Process the raft group and make sure its running if needed.
	js.createRaftGroup(rg)

	// Go ahead and create or update the stream.
	mset, err := acc.LookupStream(sa.Config.Name)
	if err == nil && mset != nil {
		if err := mset.Update(sa.Config); err != nil {
			s.Warnf("JetStream cluster error updating stream %q for account %q: %v", sa.Config.Name, acc.Name, err)
			sa.err = err
		}
	} else {
		// Add in the stream here.
		mset, sa.err = acc.addStream(sa.Config, nil, sa)
		// Start our monitoring routine.
		if rg.node != nil {
			s.startGoRoutine(func() { js.monitorStreamRaftGroup(mset, sa) })
		} else {
			// Single replica stream, process manually here.
			js.processStreamLeaderChange(mset, sa, true)
		}
	}
}

func (js *jetStream) processClusterDeleteStream(sa *streamAssignment, wasLeader bool) {
	if sa == nil {
		return
	}
	fmt.Printf("[%s] Stream Removal Recording\n", js.srv.Name())
	fmt.Printf("sa.Config is %+v\n", sa.Config)

	js.mu.RLock()
	s := js.srv
	js.mu.RUnlock()

	acc, err := s.LookupAccount(sa.Client.Account)
	if err != nil {
		s.Debugf("JetStream cluster failed to lookup account %q: %v", sa.Client.Account, err)
		return
	}

	fmt.Printf("[%s:%s]\tWill do stream delete. wasLeader? %v\n", js.srv, js.nodeID(), wasLeader)
	fmt.Printf("[%s:%s]\tGroup is %+v\n", js.srv, js.nodeID(), sa.Group)

	// Go ahead and delete the stream.
	mset, err := acc.LookupStream(sa.Config.Name)
	if err == nil && mset != nil {
		if mset.Config().internal {
			err = errors.New("not allowed to delete internal stream")
		} else {
			err = mset.Delete()
		}
	}

	if !wasLeader {
		return
	}

	var resp = JSApiStreamDeleteResponse{ApiResponse: ApiResponse{Type: JSApiStreamDeleteResponseType}}
	if err != nil {
		resp.Error = jsError(err)
	} else {
		resp.Success = true
	}
	fmt.Printf("[%s:%s]\tSENDING API RESPONSE TO STREAM DELETE!!\n", s.Name(), s.js.nodeID())
	s.sendAPIResponse(sa.Client, acc, _EMPTY_, sa.Reply, _EMPTY_, s.jsonResponse(resp))
}

// processConsumerAssignment is called when followers have replicated an assignment for a consumer.
func (js *jetStream) processConsumerAssignment(ca *consumerAssignment) {
	fmt.Printf("[%s] Got a consumer assigment %+v\n", js.srv.Name(), ca)

	js.mu.Lock()
	s, cc := js.srv, js.cluster
	if s == nil || cc == nil {
		// TODO(dlc) - debug at least
		js.mu.Unlock()
		return
	}

	sa := js.streamAssignment(ca.Client.Account, ca.Stream)
	if sa == nil {
		// FIXME(dlc) - log.
		js.mu.Unlock()
		return
	}
	fmt.Printf("[%s] related sa is %+v\n", js.srv, sa)

	if sa.consumers == nil {
		sa.consumers = make(map[string]*consumerAssignment)
	}

	// Place into our internal map under the stream assignment.
	// Ok to replace an existing one, we check on process call below.
	sa.consumers[ca.Name] = ca
	// See if we are a member
	isMember := ca.Group.isMember(cc.meta.ID())
	js.mu.Unlock()

	// Check if this is for us..
	if isMember {
		fmt.Printf("Will process since we are a member!!\n")
		js.processClusterCreateConsumer(ca)
	}
}

func (js *jetStream) processConsumerRemoval(ca *consumerAssignment) {
	fmt.Printf("[%s] Got a consumer removal %+v\n", js.srv, ca)
	js.mu.RLock()
	s, cc := js.srv, js.cluster
	js.mu.RUnlock()
	if s == nil || cc == nil {
		// TODO(dlc) - debug at least
		return
	}
	// Delete from our state.
	js.mu.Lock()
	wasLeader := cc.isConsumerLeader(ca.Client.Account, ca.Stream, ca.Name)
	if accStreams := cc.streams[ca.Client.Account]; accStreams != nil {
		if sa := accStreams[ca.Stream]; sa != nil && sa.consumers != nil {
			delete(sa.consumers, ca.Name)
		}
	}
	js.mu.Unlock()

	fmt.Printf("Will process remove consumer regardless!!\n")
	js.processClusterDeleteConsumer(ca, wasLeader)
}

// processClusterCreateConsumer is when we are a member fo the group and need to create the consumer.
func (js *jetStream) processClusterCreateConsumer(ca *consumerAssignment) {
	if ca == nil {
		return
	}
	fmt.Printf("[%s] Consumer Assignment Recording\n", js.srv.Name())
	fmt.Printf("ca.Config is %+v\n", ca.Config)

	js.mu.RLock()
	s := js.srv
	acc, err := s.LookupAccount(ca.Client.Account)
	if err != nil {
		s.Debugf("JetStream cluster failed to lookup account %q: %v", ca.Client.Account, err)
		js.mu.RUnlock()
		return
	}
	rg := ca.Group
	js.mu.RUnlock()

	// Go ahead and create or update the consumer.
	mset, err := acc.LookupStream(ca.Stream)
	if err != nil {
		s.Debugf("JetStream cluster error looking up stream %q for account %q: %v", ca.Stream, acc.Name, err)
		ca.err = err
		return
	}

	// Check if we already have this consumer running.
	if o := mset.LookupConsumer(ca.Name); o != nil {
		if o.isDurable() && o.isPushMode() {
			ocfg := o.Config()
			if configsEqualSansDelivery(ocfg, *ca.Config) && (ocfg.allowNoInterest || o.hasNoLocalInterest()) {
				o.updateDeliverSubject(ca.Config.DeliverSubject)
			}
		}
		s.Debugf("JetStream cluster, consumer already running")
		fmt.Printf("\n**Consumer already running, not processing\n")
		return
	}

	fmt.Printf("\n**Adding in consumer with rg %+v\n", rg)

	// Process the raft group and make sure its running if needed.
	js.createRaftGroup(rg)

	// Add in the consumer.
	o, err := mset.addConsumer(ca.Config, ca.Name, rg.node)
	if err != nil {
		fmt.Printf("\n**ERROR in consumer: %v\n", err)
		ca.err = err
	}

	// Start our monitoring routine.
	if rg.node != nil {
		s.startGoRoutine(func() { js.monitorConsumerRaftGroup(o, ca) })
	} else {
		// Single replica consumer, process manually here.
		js.processConsumerLeaderChange(o, ca, true)
	}
}

func (js *jetStream) processClusterDeleteConsumer(ca *consumerAssignment, wasLeader bool) {
	if ca == nil {
		return
	}
	fmt.Printf("[%s] Consumer Removal Recording\n", js.srv.Name())

	js.mu.RLock()
	s := js.srv
	js.mu.RUnlock()

	acc, err := s.LookupAccount(ca.Client.Account)
	if err != nil {
		s.Warnf("JetStream cluster failed to lookup account %q: %v", ca.Client.Account, err)
		return
	}

	fmt.Printf("[%s:%s]\tWill do consumer delete. wasLeader? %v\n", js.srv.Name(), js.nodeID(), wasLeader)

	// Go ahead and delete the stream.
	mset, err := acc.LookupStream(ca.Stream)
	if err == nil && mset != nil {
		if mset.Config().internal {
			err = errors.New("not allowed to delete internal consumer")
		} else if obs := mset.LookupConsumer(ca.Name); obs != nil {
			err = obs.Delete()
		}
	}

	if !wasLeader {
		return
	}

	var resp = JSApiConsumerDeleteResponse{ApiResponse: ApiResponse{Type: JSApiConsumerDeleteResponseType}}
	if err != nil {
		resp.Error = jsError(err)
	} else {
		resp.Success = true
	}

	fmt.Printf("[%s:%s]\tSENDING API RESPONSE TO CONSUMER DELETE!!\n", s.Name(), s.js.nodeID())
	s.sendAPIResponse(ca.Client, acc, _EMPTY_, ca.Reply, _EMPTY_, s.jsonResponse(resp))
}

// consumerAssigned informs us if this server has this consumer assigned.
func (jsa *jsAccount) consumerAssigned(stream, consumer string) bool {
	jsa.mu.RLock()
	defer jsa.mu.RUnlock()

	js, acc := jsa.js, jsa.account
	if js == nil {
		return false
	}
	return js.cluster.isConsumerAssigned(acc, stream, consumer)
}

// Read lock should be held.
func (cc *jetStreamCluster) isConsumerAssigned(a *Account, stream, consumer string) bool {
	// Non-clustered mode always return true.
	if cc == nil {
		return true
	}
	var sa *streamAssignment
	accStreams := cc.streams[a.Name]
	if accStreams != nil {
		sa = accStreams[stream]
	}
	if sa == nil {
		// TODO(dlc) - This should not happen.
		return false
	}
	ca := sa.consumers[consumer]
	if ca == nil {
		return false
	}
	rg := ca.Group
	// Check if we are the leader of this raftGroup assigned to the stream.
	ourID := cc.meta.ID()
	for _, peer := range rg.Peers {
		if peer == ourID {
			return true
		}
	}
	return false
}

func (o *Consumer) raftNode() RaftNode {
	if o == nil {
		return nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.node
}

func (js *jetStream) monitorConsumerRaftGroup(o *Consumer, ca *consumerAssignment) {
	fmt.Printf("[%s:%s] Starting consumer monitor raft group routine\n", js.srv, ca.Group.Name)
	defer fmt.Printf("[%s:%s] Exiting consumer monitor raft group routine\n", js.srv, ca.Group.Name)

	s, n := js.server(), o.raftNode()
	if n == nil {
		s.Warnf("JetStream cluster can't monitor consumer raft group, account %q, consumer %q", o.acc.Name, o.name)
		fmt.Printf("[%s:%s] JetStream cluster can't monitor consumer raft group, account %q, consumer %q\n\n", js.srv, ca.Group.Name, o.acc.Name, o.name)
		return
	}
	qch, lch, ach := n.QuitC(), n.LeadChangeC(), n.ApplyC()

	defer s.grWG.Done()

	for {
		select {
		case <-s.quitCh:
			return
		case <-qch:
			return
		case ce := <-ach:
			// FIXME(dlc) - capture errors.
			js.applyConsumerEntries(o, ce)
			n.Applied(ce.Index)
		case isLeader := <-lch:
			js.processConsumerLeaderChange(o, ca, isLeader)
		}
	}
}

func (js *jetStream) applyConsumerEntries(o *Consumer, ce *CommittedEntry) {
	fmt.Printf("[%s] JS GROUP %q HAS CONSUMER ENTRIES UPDATE TO APPLY!\n", js.srv, o.node.Group())
	for _, e := range ce.Entries {
		if e.Type == EntrySnapshot {
			fmt.Printf("[%s] SNAPSHOT CONSUMER ENTRY\n", js.srv)
		} else {
			buf := e.Data
			switch entryOp(buf[0]) {
			case updateDeliveredOp:
				dseq, sseq, dc, ts, err := decodeDeliveredUpdate(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				if err := o.store.UpdateDelivered(dseq, sseq, dc, ts); err != nil {
					panic(err.Error())
				}
			case updateAcksOp:
				dseq, sseq, err := decodeAckUpdate(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				if err := o.store.UpdateAcks(dseq, sseq); err != nil {
					panic(err.Error())
				}
			case updateFullStateOp:
				state, err := decodeConsumerState(buf[1:])
				if err != nil {
					panic(err.Error())
				}
				fmt.Printf("\n\nDECODE OF FULL STATE IS %+v\n", state)
				o.store.Update(state)
				// We can compact here since this is our complete state.
				// FIXME(dlc) - Need index though.
				//o.node.Compact(ce.Index)
			default:
				fmt.Printf("OP is %v\n", buf[0])
				panic("JetStream Cluster Unknown group entry op type!")
			}
		}
	}
}

var errBadAckUpdate = errors.New("jetstream cluster bad replicated ack update")
var errBadDeliveredUpdate = errors.New("jetstream cluster bad replicated delivered update")

func decodeAckUpdate(buf []byte) (dseq, sseq uint64, err error) {
	var bi, n int
	if dseq, n = binary.Uvarint(buf); n < 0 {
		return 0, 0, errBadAckUpdate
	}
	bi += n
	if sseq, n = binary.Uvarint(buf[bi:]); n < 0 {
		return 0, 0, errBadAckUpdate
	}
	return dseq, sseq, nil
}

func decodeDeliveredUpdate(buf []byte) (dseq, sseq, dc uint64, ts int64, err error) {
	var bi, n int
	if dseq, n = binary.Uvarint(buf); n < 0 {
		return 0, 0, 0, 0, errBadDeliveredUpdate
	}
	bi += n
	if sseq, n = binary.Uvarint(buf[bi:]); n < 0 {
		return 0, 0, 0, 0, errBadDeliveredUpdate
	}
	bi += n
	if dc, n = binary.Uvarint(buf[bi:]); n < 0 {
		return 0, 0, 0, 0, errBadDeliveredUpdate
	}
	bi += n
	if ts, n = binary.Varint(buf[bi:]); n < 0 {
		return 0, 0, 0, 0, errBadDeliveredUpdate
	}
	return dseq, sseq, dc, ts, nil
}

func (js *jetStream) processConsumerLeaderChange(o *Consumer, ca *consumerAssignment, isLeader bool) {
	fmt.Printf("\n\n[%s] JS detected consumer leadership change for %q! %v\n", js.srv, ca.Group.Name, isLeader)

	o.setLeader(isLeader)

	if !isLeader {
		return
	}

	// Check if we need to respond to the original request.
	// FIXME(dlc) - need to replicate responded state or do simple signal. ok if we send twice.
	js.mu.Lock()
	responded, err := ca.responded, ca.err
	ca.responded = true
	s := js.srv
	js.mu.Unlock()

	if isLeader && !responded {
		acc := o.account()
		var resp = JSApiConsumerCreateResponse{ApiResponse: ApiResponse{Type: JSApiConsumerCreateResponseType}}
		if err != nil {
			resp.Error = jsError(err)
		} else {
			fmt.Printf("\n\n[%s] - Successfully created our consumer!!! %+v\n\n", s, o)
			fmt.Printf("s is %q, acc is %q\n", s.Name(), acc.Name)
			resp.ConsumerInfo = o.Info()
		}
		s.sendAPIResponse(ca.Client, acc, _EMPTY_, ca.Reply, _EMPTY_, s.jsonResponse(&resp))
	}
}

func (js *jetStream) processLeaderChange(isLeader bool) {
	fmt.Printf("[%s] JS detected leadership change! %v\n", js.srv, isLeader)

	js.mu.Lock()
	defer js.mu.Unlock()

	fmt.Printf("[%s] Processing leader change!!\n\n", js.srv)

	if !isLeader {
		// TODO(dlc) - stepdown.
		return
	}
}

// selectPeerGroup will select a group of peers to start a raft group.
// TODO(dlc) - For now randomly select. Can be way smarter.
func (cc *jetStreamCluster) selectPeerGroup(r int) []string {
	var nodes []string
	peers := cc.meta.Peers()
	// Make sure they are active
	s := cc.s
	ourID := cc.meta.ID()
	for _, p := range peers {
		if p.ID == ourID || s.getRouteByHash([]byte(p.ID)) != nil {
			nodes = append(nodes, p.ID)
		} else {
			fmt.Printf("peer %q not online!\n", p.ID)
		}
	}
	if len(nodes) < r {
		fmt.Printf("Not enough active peers! %d\n", len(nodes))
		return nil
	}
	// Don't depend on range.
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
	return nodes[:r]
}

func groupNameForStream(peers []string, storage StorageType) string {
	return groupName("S", peers, storage)
}

func groupNameForConsumer(peers []string, storage StorageType) string {
	return groupName("C", peers, storage)
}

func groupName(prefix string, peers []string, storage StorageType) string {
	var gns string
	if len(peers) == 1 {
		gns = peers[0]
	} else {
		gns = string(getHash(nuid.Next()))
	}
	return fmt.Sprintf("%s-R%d%s-%s", prefix, len(peers), storage.String()[:1], gns)
}

// createGroupForStream will create a group for assignment for the stream.
// Lock should be held.
func (cc *jetStreamCluster) createGroupForStream(cfg *StreamConfig) *raftGroup {
	replicas := cfg.Replicas
	if replicas == 0 {
		replicas = 1
	}

	// Need to create a group here.
	// TODO(dlc) - Can be way smarter here.
	peers := cc.selectPeerGroup(replicas)
	if len(peers) == 0 {
		return nil
	}
	return &raftGroup{Name: groupNameForStream(peers, cfg.Storage), Storage: cfg.Storage, Peers: peers}
}

func syncSubjForStream() string {
	var b [replySuffixLen]byte
	rn := rand.Int63()
	for i, l := 0, rn; i < len(b); i++ {
		b[i] = digits[l%base]
		l /= base
	}
	return fmt.Sprintf("$SYS.JSC.SYNC.%s", b[:])
}

func (s *Server) jsClusteredStreamRequest(ci *ClientInfo, subject, reply string, rmsg []byte, cfg *StreamConfig) {
	fmt.Printf("[%s:%s]\tWill answer stream create!\n", s.Name(), s.js.nodeID())
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	// Raft group selection and placement.
	rg := cc.createGroupForStream(cfg)
	if rg == nil {
		fmt.Printf("[%s:%s]\tNo group selected!\n", s.Name(), s.js.nodeID())
		acc, _ := s.LookupAccount(ci.Account)
		var resp = JSApiStreamCreateResponse{ApiResponse: ApiResponse{Type: JSApiStreamCreateResponseType}}
		resp.Error = jsInsufficientErr
		s.sendAPIResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}
	// Sync subject for post snapshot sync.
	sync := syncSubjForStream()

	sa := &streamAssignment{Group: rg, Sync: sync, Config: cfg, Reply: reply, Client: ci}
	cc.meta.Propose(encodeAddStreamAssignment(sa))
}

func (s *Server) jsClusteredStreamDeleteRequest(ci *ClientInfo, stream, subject, reply string, rmsg []byte) {
	fmt.Printf("[%s:%s]\tWill answer stream delete!\n", s.Name(), s.js.nodeID())
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	osa := js.streamAssignment(ci.Account, stream)
	if osa == nil {
		// TODO(dlc) - Should respond? Log?
		return
	}
	sa := &streamAssignment{Group: osa.Group, Config: osa.Config, Reply: reply, Client: ci}
	cc.meta.Propose(encodeDeleteStreamAssignment(sa))
}

func (s *Server) jsClusteredStreamPurgeRequest(ci *ClientInfo, stream, subject, reply string, rmsg []byte) {
	fmt.Printf("[%s:%s]\tWill answer stream purge!\n", s.Name(), s.js.nodeID())
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	sa := js.streamAssignment(ci.Account, stream)
	if sa == nil || sa.Group == nil || sa.Group.node == nil {
		// TODO(dlc) - Should respond? Log?
		return
	}
	n := sa.Group.node
	fmt.Printf("SA is %+v\n", sa.Group)
	sp := &streamPurge{Stream: stream, Reply: reply, Client: ci}
	n.Propose(encodeStreamPurge(sp))
}

func encodeStreamPurge(sp *streamPurge) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(purgeStreamOp))
	json.NewEncoder(&bb).Encode(sp)
	return bb.Bytes()
}

func decodeStreamPurge(buf []byte) (*streamPurge, error) {
	var sp streamPurge
	err := json.Unmarshal(buf, &sp)
	return &sp, err
}

func (s *Server) jsClusteredConsumerDeleteRequest(ci *ClientInfo, stream, consumer, subject, reply string, rmsg []byte) {
	fmt.Printf("[%s:%s]\tWill answer consumer delete!\n", s.Name(), s.js.nodeID())
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	sa := js.streamAssignment(ci.Account, stream)
	if sa == nil || sa.consumers == nil {
		// TODO(dlc) - Should respond? Log?
		return
	}
	oca := sa.consumers[consumer]
	if oca == nil {
		// TODO(dlc) - Should respond? Log?
		return
	}
	fmt.Printf("CA is %+v\n", oca)
	ca := &consumerAssignment{Group: oca.Group, Stream: stream, Name: consumer, Config: oca.Config, Reply: reply, Client: ci}
	cc.meta.Propose(encodeDeleteConsumerAssignment(ca))
}

func encodeMsgDelete(md *streamMsgDelete) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(deleteMsgOp))
	json.NewEncoder(&bb).Encode(md)
	return bb.Bytes()
}

func decodeMsgDelete(buf []byte) (*streamMsgDelete, error) {
	var md streamMsgDelete
	err := json.Unmarshal(buf, &md)
	return &md, err
}

func (s *Server) jsClusteredMsgDeleteRequest(ci *ClientInfo, stream, subject, reply string, seq uint64, rmsg []byte) {
	fmt.Printf("[%s:%s]\tWill answer stream msg delete %d!\n", s, s.js.nodeID(), seq)
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	sa := js.streamAssignment(ci.Account, stream)
	if sa == nil || sa.Group == nil || sa.Group.node == nil {
		// TODO(dlc) - Should respond? Log?
		return
	}
	n := sa.Group.node
	md := &streamMsgDelete{Seq: seq, Stream: stream, Reply: reply, Client: ci}
	err := n.Propose(encodeMsgDelete(md))

	fmt.Printf("[%s:%s]\tDONE PROPOSE FOR stream msg delete %d - %v!\n", s, s.js.nodeID(), seq, err)
}

func encodeAddStreamAssignment(sa *streamAssignment) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(assignStreamOp))
	json.NewEncoder(&bb).Encode(sa)
	return bb.Bytes()
}

func encodeDeleteStreamAssignment(sa *streamAssignment) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(removeStreamOp))
	json.NewEncoder(&bb).Encode(sa)
	return bb.Bytes()
}

func decodeStreamAssignment(buf []byte) (*streamAssignment, error) {
	var sa streamAssignment
	err := json.Unmarshal(buf, &sa)
	return &sa, err
}

// createGroupForConsumer will create a new group with same peer set as the stream.
func (cc *jetStreamCluster) createGroupForConsumer(sa *streamAssignment) *raftGroup {
	peers := sa.Group.Peers
	if len(peers) == 0 {
		return nil
	}
	return &raftGroup{Name: groupNameForConsumer(peers, sa.Config.Storage), Storage: sa.Config.Storage, Peers: peers}
}

func (s *Server) jsClusteredConsumerRequest(ci *ClientInfo, subject, reply string, rmsg []byte, stream string, cfg *ConsumerConfig) {
	fmt.Printf("[%s:%s]\tWill answer consumer create!\n", s.Name(), s.js.nodeID())
	js, cc := s.getJetStreamCluster()
	if js == nil || cc == nil {
		return
	}

	js.mu.Lock()
	defer js.mu.Unlock()

	fmt.Printf("[%s:%s]\tcfg is %+v!\n", s.Name(), s.js.nodeID(), cfg)
	fmt.Printf("[%s:%s]\tstream is %v!\n", s.Name(), s.js.nodeID(), stream)

	// Lookup the stream assignment.
	sa := js.streamAssignment(ci.Account, stream)
	if sa == nil {
		fmt.Printf("[%s:%s]\tNo stream for consumer!\n", s.Name(), s.js.nodeID())
		acc, _ := s.LookupAccount(ci.Account)
		var resp = JSApiStreamCreateResponse{ApiResponse: ApiResponse{Type: JSApiStreamCreateResponseType}}
		resp.Error = jsError(ErrJetStreamStreamNotFound)
		s.sendAPIResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}
	fmt.Printf("[%s:%s]\tsa is %+v!\n", s.Name(), s.js.nodeID(), sa)

	rg := cc.createGroupForConsumer(sa)
	fmt.Printf("[%s:%s]\trg is %+v!\n", s.Name(), s.js.nodeID(), rg)

	if rg == nil {
		fmt.Printf("[%s:%s]\tNo group selected for consumer!\n", s.Name(), s.js.nodeID())
		acc, _ := s.LookupAccount(ci.Account)
		var resp = JSApiStreamCreateResponse{ApiResponse: ApiResponse{Type: JSApiStreamCreateResponseType}}
		resp.Error = jsInsufficientErr
		s.sendAPIResponse(ci, acc, subject, reply, string(rmsg), s.jsonResponse(&resp))
		return
	}

	// We need to set the ephemeral here before replicating.
	var oname string
	if !isDurableConsumer(cfg) {
		for {
			oname = createConsumerName()
			if sa.consumers != nil {
				if sa.consumers[oname] != nil {
					continue
				}
			}
			break
		}
	} else {
		oname = cfg.Durable
	}

	ca := &consumerAssignment{Group: rg, Stream: stream, Name: oname, Config: cfg, Reply: reply, Client: ci}
	fmt.Printf("[%s:%s]\tca is %+v!\n", s.Name(), s.js.nodeID(), ca)
	cc.meta.Propose(encodeAddConsumerAssignment(ca))
}

func encodeAddConsumerAssignment(ca *consumerAssignment) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(assignConsumerOp))
	json.NewEncoder(&bb).Encode(ca)
	return bb.Bytes()
}

func encodeDeleteConsumerAssignment(ca *consumerAssignment) []byte {
	var bb bytes.Buffer
	bb.WriteByte(byte(removeConsumerOp))
	json.NewEncoder(&bb).Encode(ca)
	return bb.Bytes()
}

func decodeConsumerAssignment(buf []byte) (*consumerAssignment, error) {
	var ca consumerAssignment
	err := json.Unmarshal(buf, &ca)
	return &ca, err
}

var errBadStreamMsg = errors.New("jetstream cluster bad replicated stream msg")

func decodeStreamMsg(buf []byte) (subject, reply string, hdr, msg []byte, lseq uint64, ts int64, err error) {
	var le = binary.LittleEndian
	if len(buf) < 26 {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	lseq = le.Uint64(buf)
	buf = buf[8:]
	ts = int64(le.Uint64(buf))
	buf = buf[8:]
	sl := int(le.Uint16(buf))
	buf = buf[2:]
	if len(buf) < sl {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	subject = string(buf[:sl])
	buf = buf[sl:]
	if len(buf) < 2 {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	rl := int(le.Uint16(buf))
	buf = buf[2:]
	if len(buf) < rl {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	reply = string(buf[:rl])
	buf = buf[rl:]
	if len(buf) < 2 {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	hl := int(le.Uint16(buf))
	buf = buf[2:]
	if len(buf) < hl {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	hdr = buf[:hl]
	buf = buf[hl:]
	if len(buf) < 4 {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	ml := int(le.Uint32(buf))
	buf = buf[4:]
	if len(buf) < ml {
		return _EMPTY_, _EMPTY_, nil, nil, 0, 0, errBadStreamMsg
	}
	msg = buf[:ml]
	return subject, reply, hdr, msg, lseq, ts, nil
}

func encodeStreamMsg(subject, reply string, hdr, msg []byte, lseq uint64, ts int64) []byte {
	elen := 1 + 8 + 8 + len(subject) + len(reply) + len(hdr) + len(msg)
	elen += (2 + 2 + 2 + 4) // Encoded lengths, 4bytes
	// TODO(dlc) - check sizes of subject, reply and hdr, make sure uint16 ok.
	buf := make([]byte, elen)
	buf[0] = byte(streamMsgOp)
	var le = binary.LittleEndian
	wi := 1
	le.PutUint64(buf[wi:], lseq)
	wi += 8
	le.PutUint64(buf[wi:], uint64(ts))
	wi += 8
	le.PutUint16(buf[wi:], uint16(len(subject)))
	wi += 2
	copy(buf[wi:], subject)
	wi += len(subject)
	le.PutUint16(buf[wi:], uint16(len(reply)))
	wi += 2
	copy(buf[wi:], reply)
	wi += len(reply)
	le.PutUint16(buf[wi:], uint16(len(hdr)))
	wi += 2
	if len(hdr) > 0 {
		copy(buf[wi:], hdr)
		wi += len(hdr)
	}
	le.PutUint32(buf[wi:], uint32(len(msg)))
	wi += 4
	if len(msg) > 0 {
		copy(buf[wi:], msg)
		wi += len(msg)
	}
	return buf[:wi]
}

// processClusteredMsg will propose the inbound message to the underlying raft group.
func (mset *Stream) processClusteredInboundMsg(subject, reply string, hdr, msg []byte) error {
	mset.mu.Lock()

	// For possible error response.
	var response []byte
	canRespond := !mset.config.NoAck && len(reply) > 0 && mset.isLeader()
	sendq := mset.sendq

	// We only use mset.nlseq for clustering and in case we run ahead of actual commits.
	// Check if we need to set initial value here
	if mset.nlseq < mset.lseq {
		mset.nlseq = mset.lseq
	}

	err := mset.node.Propose(encodeStreamMsg(subject, reply, hdr, msg, mset.nlseq, time.Now().UnixNano()))
	if err != nil {
		if canRespond {
			var resp = &JSPubAckResponse{PubAck: &PubAck{Stream: mset.config.Name}}
			resp.Error = &ApiError{Code: 503, Description: err.Error()}
			response, _ = json.Marshal(resp)
		}
	} else {
		mset.nlseq++
	}
	mset.mu.Unlock()

	// If we errored out respond here.
	if err != nil && len(response) > 0 {
		sendq <- &jsPubMsg{reply, _EMPTY_, _EMPTY_, nil, response, nil, 0}
	}

	return err
}

// For requesting messages post raft snapshot to catch up streams post server restart.
// Any deleted msgs etc will be handled inline on catchup.
type streamSyncRequest struct {
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
}

// Given a stream state that represents a snapshot, calculate the sync request based on our current state.
func (mset *Stream) calculateSyncRequest(state, snap *StreamState) *streamSyncRequest {
	// Quick check if we are already caught up.
	if state.LastSeq >= snap.LastSeq {
		return nil
	}
	fmt.Printf("[%s] CURRENT STATE  - %+v\n", mset.srv, state)
	fmt.Printf("[%s] SNAPSHOT STATE - %+v\n", mset.srv, snap)

	return &streamSyncRequest{
		FirstSeq: state.LastSeq + 1,
		LastSeq:  snap.LastSeq,
	}
}

// processSnapshotDeletes will update our current store based on the snapshot
// but only processing deletes and new FirstSeq / purges.
func (mset *Stream) processSnapshotDeletes(snap *StreamState) {
	state := mset.store.State()

	// Adjust if FirstSeq has moved.
	if snap.FirstSeq > state.FirstSeq {
		mset.store.Compact(snap.FirstSeq)
		state = mset.store.State()
	}
	// Range the deleted and delete if applicable.
	for _, dseq := range snap.Deleted {
		if dseq <= state.LastSeq {
			mset.store.RemoveMsg(dseq)
		}
	}
}

// Process a stream snapshot.
func (mset *Stream) processSnapshot(buf []byte) {
	var snap StreamState
	if err := json.Unmarshal(buf, &snap); err != nil {
		// Log error.
		return
	}

	fmt.Printf("[%s] SNAPSHOT STREAM ENTRY\n", mset.srv)

	// Update any deletes, etc.
	mset.processSnapshotDeletes(&snap)

	mset.mu.Lock()
	state := mset.store.State()
	sreq := mset.calculateSyncRequest(&state, &snap)
	if sreq != nil {
	}
	s, subject := mset.srv, mset.sa.Sync
	mset.mu.Unlock()

	fmt.Printf("[%s] SYNC REQUEST WOULD BE %+v\n", mset.srv, sreq)

	// Send our catchup request here if needed.
	if sreq != nil {
		reply := syncReplySubject()
		sub, _ := s.sysSubscribe(reply, mset.handleClusterSyncResponses)
		mset.mu.Lock()
		mset.replaySub = sub
		mset.mu.Unlock()
		s.sendInternalMsgLocked(subject, reply, nil, sreq)
	}
}

func (mset *Stream) handleClusterSyncRequest(sub *subscription, c *client, subject, reply string, msg []byte) {
	fmt.Printf("\n\n[%s] RECEIVED A SYNC/CATCHUP REQUEST - %q - %q\n", mset.srv, reply, msg)

	var sreq streamSyncRequest
	if err := json.Unmarshal(msg, &sreq); err != nil {
		// Log error.
		return
	}
	fmt.Printf("[%s] SREQ IS %+v\n", mset.srv, sreq)

	mset.srv.startGoRoutine(func() { mset.runCatchup(reply, &sreq) })
}

func (mset *Stream) runCatchup(sendSubject string, sreq *streamSyncRequest) {
	s := mset.srv
	defer s.grWG.Done()

	const maxOut = int64(48 * 1024 * 1024) // 48MB for now.
	out := int64(0)

	// Flow control processing.
	const expectedTokens = 5
	ackReplySize := func(subj string) int64 {
		tsa := [expectedTokens]string{}
		start, tokens := 0, tsa[:0]
		for i := 0; i < len(subj); i++ {
			if subj[i] == btsep {
				tokens = append(tokens, subj[start:i])
				start = i + 1
			}
		}
		tokens = append(tokens, subj[start:])
		if len(tokens) != expectedTokens {
			return 0
		}
		return parseAckReplyNum(tokens[expectedTokens-1])
	}

	nextBatchC := make(chan struct{}, 1)
	nextBatchC <- struct{}{}

	// Setup ackReply for flow control.
	ackReply := syncAckSubject()
	ackSub, _ := s.sysSubscribe(ackReply, func(sub *subscription, c *client, subject, reply string, msg []byte) {
		sz := ackReplySize(subject)
		atomic.AddInt64(&out, -sz)
		select {
		case nextBatchC <- struct{}{}:
		}
		fmt.Printf("[%s] GOT SYNC ACK REPLY! %q - %d\n", s, subject, sz)
	})
	defer s.sysUnsubscribe(ackSub)
	ackReplyT := strings.ReplaceAll(ackReply, ".*", ".%d")

	// Setup sequences to walk through.
	seq, last := sreq.FirstSeq, sreq.LastSeq

	sendNextBatch := func() {
		for ; seq < last && atomic.LoadInt64(&out) <= maxOut; seq++ {
			subj, hdr, msg, ts, err := mset.store.LoadMsg(seq)
			if err != nil {
				if err == ErrStoreEOF {
					// break, something changed.
				} else if err == ErrStoreMsgNotFound {
					// Send deleteOp/skipOp
				}
			}
			// S2?
			em := encodeStreamMsg(subj, _EMPTY_, hdr, msg, seq-1, ts)
			// Place size in reply subject for flow control.
			reply := fmt.Sprintf(ackReplyT, len(em))
			atomic.AddInt64(&out, int64(len(em)))
			s.sendInternalMsgLocked(sendSubject, reply, nil, em)
		}
	}

	const activityInterval = 500 * time.Millisecond
	timeout := time.NewTicker(activityInterval)
	defer timeout.Stop()

	// Grab stream quit channel.
	mset.mu.RLock()
	qch := mset.qch
	mset.mu.RUnlock()
	if qch == nil {
		return
	}

	// Run as long as we are still active and need catchup.
	// FIXME(dlc) - Purge event? Stream delete?
	for {
		select {
		case <-s.quitCh:
			return
		case <-qch:
			return
		case <-timeout.C:
			fmt.Printf("[%s] STREAM Catching up for %q stalled\n", s, mset.config.Name)
			return
		case <-nextBatchC:
			// Update our activity timer.
			timeout.Reset(activityInterval)
			if seq >= last {
				fmt.Printf("[%s] DONE RESYNC ON STREAM, EXITING\n", s)
				return
			}
			// Still have more catching up to do.
			sendNextBatch()
		}
	}
}

func (mset *Stream) handleClusterSyncResponses(sub *subscription, c *client, subject, reply string, msg []byte) {
	s := mset.srv

	fmt.Printf("\n\n[%s] RECEIVED A SYNC/CATCHUP RESPONSE\n", s)
	if len(msg) < 1 {
		// FIXME(dlc) - log
		return
	}

	switch entryOp(msg[0]) {
	case streamMsgOp:
		subj, _, hdr, msg, lseq, ts, err := decodeStreamMsg(msg[1:])
		if err != nil {
			// TODO(dlc) - Bail?
		}
		fmt.Printf("[%s] Received replay msg of %q %q %q %d %d\n", mset.srv, subj, hdr, msg, lseq, ts)
		// Put into store
		// For flow control.
		if reply != _EMPTY_ {
			fmt.Printf("[%s] Sending ack flow response to %q\n", mset.srv, reply)
			s.sendInternalMsgLocked(reply, _EMPTY_, nil, nil)
			fmt.Printf("[%s] DONE Sending ack flow response to %q\n", mset.srv, reply)
		}
	case deleteMsgOp:

	}

}

func syncReplySubject() string {
	var b [replySuffixLen]byte
	rn := rand.Int63()
	for i, l := 0, rn; i < len(b); i++ {
		b[i] = digits[l%base]
		l /= base
	}
	return fmt.Sprintf("$SYS.JSC.R.%s", b[:])
}

func syncAckSubject() string {
	var b [replySuffixLen]byte
	rn := rand.Int63()
	for i, l := 0, rn; i < len(b); i++ {
		b[i] = digits[l%base]
		l /= base
	}
	return fmt.Sprintf("$SYS.JSC.ACK.%s.*", b[:])
}
