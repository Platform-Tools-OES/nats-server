// Copyright 2020 The NATS Authors
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

package test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/logger"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

var jsClusterTempl = `
	listen: 127.0.0.1:-1
	server_name: %s
	jetstream: {max_mem_store: 16GB, max_file_store: 10TB, store_dir: "%s"}
	cluster {
		name: %s
		listen: 127.0.0.1:%d
		routes = [%s]
	}
`

// This will create a cluster that is explicitly configured for the routes, etc.
// and also has a defined clustername. All configs for routes and cluster name will be the same.
func createJetStreamClusterExplicit(t *testing.T, clusterName string, numServers int) *cluster {
	t.Helper()
	if clusterName == "" || numServers < 1 {
		t.Fatalf("Bad params")
	}
	const startClusterPort = 22332

	// Build out the routes that will be shared with all configs.
	var routes []string
	for cp := startClusterPort; cp < startClusterPort+numServers; cp++ {
		routes = append(routes, fmt.Sprintf("nats-route://127.0.0.1:%d", cp))
	}
	routeConfig := strings.Join(routes, ",")

	// Go ahead and build configurations and start servers.
	c := &cluster{servers: make([]*server.Server, 0, numServers), opts: make([]*server.Options, 0, numServers), name: clusterName}

	for cp := startClusterPort; cp < startClusterPort+numServers; cp++ {
		storeDir, _ := ioutil.TempDir("", server.JetStreamStoreDir)
		sn := fmt.Sprintf("S-%d", cp-startClusterPort+1)
		conf := fmt.Sprintf(jsClusterTempl, sn, storeDir, clusterName, cp, routeConfig)
		s, o := RunServerWithConfig(createConfFile(t, []byte(conf)))
		if doLog {
			pre := fmt.Sprintf("[S-%d] - ", cp-startClusterPort+1)
			s.SetLogger(logger.NewTestLogger(pre, true), true, true)
		}
		c.servers = append(c.servers, s)
		c.opts = append(c.opts, o)
	}
	c.t = t

	// Wait til we are formed and have a leader.
	c.checkClusterFormed()
	c.waitOnClusterReady()

	fmt.Printf("\n\nCLUSTER FORMED AND READY!\n\n")

	return c
}

func (c *cluster) addInNewServer() *server.Server {
	c.t.Helper()
	sn := fmt.Sprintf("S-%d", len(c.servers)+1)
	storeDir, _ := ioutil.TempDir("", server.JetStreamStoreDir)
	seedRoute := fmt.Sprintf("nats-route://127.0.0.1:%d", c.opts[0].Cluster.Port)
	conf := fmt.Sprintf(jsClusterTempl, sn, storeDir, c.name, -1, seedRoute)
	s, o := RunServerWithConfig(createConfFile(c.t, []byte(conf)))
	if doLog {
		pre := fmt.Sprintf("[%s] - ", sn)
		s.SetLogger(logger.NewTestLogger(pre, true), true, true)
	}
	c.servers = append(c.servers, s)
	c.opts = append(c.opts, o)
	c.checkClusterFormed()
	return s
}

// Hack for staticcheck
var skip = func(t *testing.T) {
	t.SkipNow()
}

func TestJetStreamClusterConfig(t *testing.T) {
	conf := createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		jetstream: {max_mem_store: 16GB, max_file_store: 10TB, store_dir: "%s"}
		cluster { listen: 127.0.0.1:-1 }
	`))
	defer os.Remove(conf)

	check := func(errStr string) {
		t.Helper()
		opts, err := server.ProcessConfigFile(conf)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if _, err := server.NewServer(opts); err == nil || !strings.Contains(err.Error(), errStr) {
			t.Fatalf("Expected an error of `%s`, got `%v`", errStr, err)
		}
	}

	check("requires `server_name`")

	conf = createConfFile(t, []byte(`
		listen: 127.0.0.1:-1
		server_name: "TEST"
		jetstream: {max_mem_store: 16GB, max_file_store: 10TB, store_dir: "%s"}
		cluster { listen: 127.0.0.1:-1 }
	`))
	defer os.Remove(conf)

	check("requires `cluster_name`")
}

func TestJetStreamClusterLeader(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "JSC", 3)
	defer c.shutdown()

	// Kill our current leader and force and election.
	c.leader().Shutdown()
	c.waitOnClusterReady()

	// Now killing our current leader should leave us leaderless.
	c.leader().Shutdown()
	c.expectNoLeader()
}

func TestJetStreamExpandCluster(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "JSC", 3)
	defer c.shutdown()

	fmt.Printf("\nEXPANDING THE CLUSTER\n\n")
	c.addInNewServer()

	fmt.Printf("\nDONE EXPANDING THE CLUSTER\n\n")
	c.waitOnPeerCount(4)
	fmt.Printf("\nDONE WAITING ON EXPANDING THE CLUSTER\n\n")
}

func TestJetStreamClusterAccountInfo(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "JSC", 3)
	defer c.shutdown()

	nc := clientConnectToServer(t, c.randomServer())
	defer nc.Close()

	reply := nats.NewInbox()
	sub, _ := nc.SubscribeSync(reply)

	if err := nc.PublishRequest(server.JSApiAccountInfo, reply, nil); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	checkSubsPending(t, sub, 1)
	resp, _ := sub.NextMsg(0)

	var info server.JSApiAccountInfoResponse
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if info.JetStreamAccountStats == nil || info.Error != nil {
		t.Fatalf("Did not receive correct response: %+v", info.Error)
	}
	fmt.Printf("info is %+v\n", info.JetStreamAccountStats)

	// Make sure we only got 1 response.
	// Technicall this will always work since its a singelton service export.
	if nmsgs, _, _ := sub.Pending(); nmsgs > 0 {
		t.Fatalf("Expected only a single response, got %d more", nmsgs)
	}
}

func jsClientConnect(t *testing.T, s *server.Server) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("Unexpected error getting JetStream context: %v", err)
	}
	return nc, js
}

func checkSubsPending(t *testing.T, sub *nats.Subscription, numExpected int) {
	t.Helper()
	checkFor(t, 200*time.Millisecond, 10*time.Millisecond, func() error {
		if nmsgs, _, err := sub.Pending(); err != nil || nmsgs != numExpected {
			return fmt.Errorf("Did not receive correct number of messages: %d vs %d", nmsgs, numExpected)
		}
		return nil
	})
}

func TestJetStreamClusterSingleReplicaStreams(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R1S", 3)
	defer c.shutdown()

	sc := &server.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo", "bar"},
	}
	// Make sure non-leaders error if directly called.
	s := c.randomNonLeader()
	if _, err := s.GlobalAccount().AddStream(sc); err == nil {
		t.Fatalf("Expected an error from a non-leader")
	}

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	fmt.Printf("Adding stream!\n\n")

	si, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo", "bar"},
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	fmt.Printf("si is %+v\n", si)

	// Send in 10 messages.
	msg, toSend := []byte("Hello JS Clustering"), 10
	for i := 0; i < toSend; i++ {
		if _, err = js.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}
	fmt.Printf("\nREQUESTING STREAM INFO\n\n")

	// Now grab info for this stream.
	si, err = js.StreamInfo("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if si == nil || si.Config.Name != "TEST" {
		t.Fatalf("StreamInfo is not correct %+v", si)
	}
	// Check active state as well, shows that the owner answered.
	if si.State.Msgs != uint64(toSend) {
		t.Fatalf("Expected %d msgs, got bad state: %+v", toSend, si.State)
	}
	fmt.Printf("Received an SI of %+v\n", si)

	// Now create a consumer. This should be pinned to same server that our stream was allocated to.
	fmt.Printf("\nCREATING CONSUMER THROUGH SUBSCRIBE\n\n")

	// First do a normal sub.
	sub, err := js.SubscribeSync("foo")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	checkSubsPending(t, sub, toSend)

	fmt.Printf("\nCREATING CONSUMER\n\n")

	// Now create a consumer as well.
	ci, err := js.AddConsumer("TEST", &nats.ConsumerConfig{Durable: "dlc", AckPolicy: nats.AckExplicit})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if ci == nil || ci.Name != "dlc" || ci.Stream != "TEST" {
		t.Fatalf("ConsumerInfo is not correct %+v", ci)
	}
}

func TestJetStreamClusterMultiReplicaStreams(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "RNS", 5)
	defer c.shutdown()

	s := c.randomServer()

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	fmt.Printf("Adding stream!\n\n")

	// FIXME(dlc) - This should be default.
	si, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo", "bar"},
		Replicas: 3,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	fmt.Printf("si is %+v\n", si)

	fmt.Printf("\nSENDING MSG!!\n\n")

	start := time.Now()

	// Send in 10 messages.
	msg, toSend := []byte("Hello JS Clustering"), 10
	for i := 0; i < toSend; i++ {
		ts := time.Now()
		if _, err := nc.Request("foo", msg, time.Second); err != nil {
			//if _, err = js.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
		fmt.Printf("Took %v to send msg\n", time.Since(ts))
	}

	fmt.Printf("Took %v to send %d msgs with replica 2!\n\n", time.Since(start), toSend)

	fmt.Printf("\nREQUESTING STREAM INFO\n\n")

	// Now grab info for this stream.
	si, err = js.StreamInfo("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if si == nil || si.Config.Name != "TEST" {
		t.Fatalf("StreamInfo is not correct %+v", si)
	}
	// Check active state as well, shows that the owner answered.
	if si.State.Msgs != uint64(toSend) {
		t.Fatalf("Expected %d msgs, got bad state: %+v", toSend, si.State)
	}
	fmt.Printf("Received an SI of %+v\n", si)

	// Now create a consumer. This should be affinitize to the same set of servers as the stream.
	fmt.Printf("\nCREATING CONSUMER THROUGH SUBSCRIBE\n\n")

	// First do a normal sub.
	sub, err := js.SubscribeSync("foo")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	checkSubsPending(t, sub, toSend)

	fmt.Printf("\nCREATING CONSUMER\n\n")

	// Now create a consumer as well.
	ci, err := js.AddConsumer("TEST", &nats.ConsumerConfig{Durable: "dlc", AckPolicy: nats.AckExplicit})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if ci == nil || ci.Name != "dlc" || ci.Stream != "TEST" || ci.NumPending != uint64(toSend) {
		t.Fatalf("ConsumerInfo is not correct %+v", ci)
	}

	fmt.Printf("\nCI is %+v\n\n", ci)
}

func TestJetStreamClusterDelete(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "RNS", 3)
	defer c.shutdown()

	s := c.randomServer()
	// Client for API requests.
	nc := clientConnectToServer(t, s)
	defer nc.Close()

	cfg := server.StreamConfig{
		Name:     "C22",
		Subjects: []string{"foo", "bar", "baz"},
		Replicas: 2,
		Storage:  server.FileStorage,
		MaxMsgs:  100,
	}
	req, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	resp, _ := nc.Request(fmt.Sprintf(server.JSApiStreamCreateT, cfg.Name), req, time.Second)
	var scResp server.JSApiStreamCreateResponse
	if err := json.Unmarshal(resp.Data, &scResp); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if scResp.StreamInfo == nil || scResp.Error != nil {
		t.Fatalf("Did not receive correct response: %+v", scResp.Error)
	}
	fmt.Printf("SI is %+v\n", scResp.StreamInfo)

	// Now create a consumer.
	obsReq := server.CreateConsumerRequest{
		Stream: cfg.Name,
		Config: server.ConsumerConfig{Durable: "dlc", AckPolicy: server.AckExplicit},
	}
	req, err = json.Marshal(obsReq)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	resp, err = nc.Request(fmt.Sprintf(server.JSApiDurableCreateT, cfg.Name, "dlc"), req, time.Second)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	var ccResp server.JSApiConsumerCreateResponse
	if err = json.Unmarshal(resp.Data, &ccResp); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if ccResp.ConsumerInfo == nil || ccResp.Error != nil {
		t.Fatalf("Did not receive correct response: %+v", ccResp.Error)
	}
	fmt.Printf("CI is %+v\n", ccResp.ConsumerInfo)

	// Now delete the consumer.
	resp, _ = nc.Request(fmt.Sprintf(server.JSApiConsumerDeleteT, cfg.Name, "dlc"), nil, time.Second)
	var cdResp server.JSApiConsumerDeleteResponse
	if err = json.Unmarshal(resp.Data, &cdResp); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !cdResp.Success || cdResp.Error != nil {
		t.Fatalf("Got a bad response %+v", cdResp)
	}

	time.Sleep(5 * time.Second)

	// Now delete the stream.
	resp, _ = nc.Request(fmt.Sprintf(server.JSApiStreamDeleteT, cfg.Name), nil, time.Second)
	var dResp server.JSApiStreamDeleteResponse
	if err = json.Unmarshal(resp.Data, &dResp); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !dResp.Success || dResp.Error != nil {
		t.Fatalf("Got a bad response %+v", dResp.Error)
	}

	// This will get the current information about usage and limits for this account.
	resp, err = nc.Request(server.JSApiAccountInfo, nil, time.Second)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	var info server.JSApiAccountInfoResponse
	if err := json.Unmarshal(resp.Data, &info); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if info.Streams != 0 {
		t.Fatalf("Expected no remaining streams, got %d", info.Streams)
	}
}

func TestJetStreamClusterStreamPurge(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R5S", 5)
	defer c.shutdown()

	s := c.randomServer()

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo", "bar"},
		Replicas: 3,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	msg, toSend := []byte("Hello JS Clustering"), 100
	for i := 0; i < toSend; i++ {
		if _, err = js.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}

	// Now grab info for this stream.
	si, err := js.StreamInfo("TEST")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// Check active state as well, shows that the owner answered.
	if si.State.Msgs != uint64(toSend) {
		t.Fatalf("Expected %d msgs, got bad state: %+v", toSend, si.State)
	}

	fmt.Printf("\nPURGING STREAM\n\n")

	// Now purge the stream.
	resp, _ := nc.Request(fmt.Sprintf(server.JSApiStreamPurgeT, "TEST"), nil, time.Second)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	var pResp server.JSApiStreamPurgeResponse
	if err = json.Unmarshal(resp.Data, &pResp); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !pResp.Success || pResp.Error != nil {
		t.Fatalf("Got a bad response %+v", pResp)
	}
	if pResp.Purged != uint64(toSend) {
		t.Fatalf("Expected %d purged, got %d", toSend, pResp.Purged)
	}
}

func TestJetStreamClusterConsumerState(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	s := c.randomServer()

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo", "bar"},
		Replicas: 3,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	msg, toSend := []byte("Hello JS Clustering"), 10
	for i := 0; i < toSend; i++ {
		if _, err = js.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}

	sub, err := js.SubscribeSync("foo", nats.Durable("dlc"), nats.Pull(1))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	checkSubsPending(t, sub, 1)

	fmt.Printf("\nGETTING 5 MSGS\n\n")

	// Pull 5 messages and ack.
	for i := 0; i < 5; i++ {
		m, err := sub.NextMsg(time.Second)
		if err != nil {
			t.Fatalf("Unexpected error getting msg %d: %v", i+1, err)
		}
		m.Ack()
	}

	ci, _ := sub.ConsumerInfo()
	if ci.AckFloor.Consumer != 5 {
		t.Fatalf("Expected ack floor of %d, got %d", 5, ci.AckFloor.Consumer)
	}
	fmt.Printf("\n\n############ CI is %+v\n\n", ci)

	fmt.Printf("\nSHUTDOWN CONSUMER LEADER %q\n\n", c.consumerLeader("$G", "TEST", "dlc"))
	c.consumerLeader("$G", "TEST", "dlc").Shutdown()
	c.waitOnNewConsumerLeader("$G", "TEST", "dlc")
	fmt.Printf("\nHAVE NEW CONSUMER LEADER %q\n\n", c.consumerLeader("$G", "TEST", "dlc"))

	nci, _ := sub.ConsumerInfo()
	fmt.Printf("\n\n############ NCI is %+v\n\n", nci)
	if nci.Delivered != ci.Delivered {
		t.Fatalf("Consumer delivered did not match after leader switch, wanted %+v, got %+v", ci.Delivered, nci.Delivered)
	}
	if nci.AckFloor != ci.AckFloor {
		t.Fatalf("Consumer ackfloor did not match after leader switch, wanted %+v, got %+v", ci.AckFloor, nci.AckFloor)
	}

	// Now make sure we can receive new messages.
	// Pull last 5.
	for i := 0; i < 5; i++ {
		m, err := sub.NextMsg(time.Second)
		if err != nil {
			t.Fatalf("Unexpected error getting msg %d: %v", i+1, err)
		}
		m.Ack()
	}
	nci, _ = sub.ConsumerInfo()
	fmt.Printf("\n\n############ NCI2 is %+v\n\n", nci)
	if nci.Delivered.Consumer != 10 || nci.Delivered.Stream != 10 {
		t.Fatalf("Received bad delivered: %+v", nci.Delivered)
	}
	if nci.AckFloor.Consumer != 10 || nci.AckFloor.Stream != 10 {
		t.Fatalf("Received bad ackfloor: %+v", nci.AckFloor)
	}
	if nci.NumAckPending != 0 {
		t.Fatalf("Received bad ackpending: %+v", nci.NumAckPending)
	}
}

func TestJetStreamClusterFullConsumerState(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	s := c.randomServer()

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	_, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo", "bar"},
		Replicas: 3,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	msg, toSend := []byte("Hello JS Clustering"), 10
	for i := 0; i < toSend; i++ {
		if _, err = js.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}

	sub, err := js.SubscribeSync("foo", nats.Durable("dlc"), nats.Pull(1))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	checkSubsPending(t, sub, 1)
	// Now purge the stream.
	nc.Request(fmt.Sprintf(server.JSApiStreamPurgeT, "TEST"), nil, time.Second)
}

func TestJetStreamClusterMetaSnapshotsAndCatchup(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	// Shut one down.
	rs := c.randomServer()
	rs.Shutdown()
	c.waitOnLeader()

	s := c.leader()

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	fmt.Printf("\n\nCREATING STREAMS\n\n")

	numStreams := 4
	// Create 4 streams
	// FIXME(dlc) - R2 make sure we place properly.
	for i := 0; i < numStreams; i++ {
		sn := fmt.Sprintf("T-%d", i+1)
		_, err := js.AddStream(&nats.StreamConfig{Name: sn})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}

	fmt.Printf("\n\nDONE CREATING STREAMS - SNAPSHOTTING & RESTARTING SERVER %q\n\n", rs)
	c.leader().JetStreamSnapshotMeta()
	time.Sleep(250 * time.Millisecond)

	rs = c.restartServer(rs)
	c.checkClusterFormed()
	c.waitOnServerCurrent(rs)

	fmt.Printf("\n\nSHUTDOWN AGAIN, DELETE STREAMS\n\n")
	rs.Shutdown()
	c.waitOnLeader()

	for i := 0; i < numStreams; i++ {
		sn := fmt.Sprintf("T-%d", i+1)
		resp, _ := nc.Request(fmt.Sprintf(server.JSApiStreamDeleteT, sn), nil, time.Second)
		var dResp server.JSApiStreamDeleteResponse
		if err := json.Unmarshal(resp.Data, &dResp); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !dResp.Success || dResp.Error != nil {
			t.Fatalf("Got a bad response %+v", dResp.Error)
		}
	}

	fmt.Printf("\n\nDONE DELETING STREAMS - RESTARTING SERVER %q\n\n", rs)

	rs = c.restartServer(rs)
	c.checkClusterFormed()
	c.waitOnServerCurrent(rs)

	fmt.Printf("\n\nSHUTTING DOWN\n\n")
}

func TestJetStreamClusterMetaSnapshotsMultiChange(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 2)
	defer c.shutdown()

	s := c.leader()

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	fmt.Printf("\n\nCREATING STREAMS AND CONSUMERS\n\n")

	// Add in 2 streams with 1 consumer each.
	if _, err := js.AddStream(&nats.StreamConfig{Name: "S1"}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	_, err := js.AddConsumer("S1", &nats.ConsumerConfig{Durable: "S1C1", AckPolicy: nats.AckExplicit})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if _, err = js.AddStream(&nats.StreamConfig{Name: "S2"}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	_, err = js.AddConsumer("S2", &nats.ConsumerConfig{Durable: "S2C1", AckPolicy: nats.AckExplicit})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Add in a new server to the group. This way we know we can delete the original streams and consumers.
	rs := c.addInNewServer()
	c.waitOnServerCurrent(rs)

	// Shut it down.
	fmt.Printf("\n\nDONE CREATING STREAMS AND CONSUMERS - STOPPING SERVER %q\n\n", rs)

	rs.Shutdown()

	fmt.Printf("\n\nCREATING STREAMS AND CONSUMERS CHANGES\n\n")

	// We want to make changes here that test each delta scenario for the meta snapshots.
	// Add new stream and consumer.
	if _, err = js.AddStream(&nats.StreamConfig{Name: "S3"}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	_, err = js.AddConsumer("S3", &nats.ConsumerConfig{Durable: "S3C1", AckPolicy: nats.AckExplicit})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// Delete stream S2
	resp, _ := nc.Request(fmt.Sprintf(server.JSApiStreamDeleteT, "S2"), nil, time.Second)
	var dResp server.JSApiStreamDeleteResponse
	if err := json.Unmarshal(resp.Data, &dResp); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !dResp.Success || dResp.Error != nil {
		t.Fatalf("Got a bad response %+v", dResp.Error)
	}
	// Delete the consumer on S1 but add another.
	resp, _ = nc.Request(fmt.Sprintf(server.JSApiConsumerDeleteT, "S1", "S1C1"), nil, time.Second)
	var cdResp server.JSApiConsumerDeleteResponse
	if err = json.Unmarshal(resp.Data, &cdResp); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !cdResp.Success || cdResp.Error != nil {
		t.Fatalf("Got a bad response %+v", cdResp)
	}
	// Add new consumer on S1
	_, err = js.AddConsumer("S1", &nats.ConsumerConfig{Durable: "S1C2", AckPolicy: nats.AckExplicit})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	fmt.Printf("\n\nDONE CHANGING STREAMS AND CONSUMERS - SNAPSHOTTING & RESTARTING SERVER %q\n\n", rs)

	c.leader().JetStreamSnapshotMeta()
	time.Sleep(250 * time.Millisecond)

	rs = c.restartServer(rs)
	c.checkClusterFormed()
	c.waitOnServerCurrent(rs)

	fmt.Printf("\n\nSHUTTING DOWN\n\n")
}

func TestJetStreamClusterStreamCatchup(t *testing.T) {
	c := createJetStreamClusterExplicit(t, "R3S", 3)
	defer c.shutdown()

	s := c.randomServer()

	// Client based API
	nc, js := jsClientConnect(t, s)
	defer nc.Close()

	fmt.Printf("\n\nCREATING STREAM\n\n")

	_, err := js.AddStream(&nats.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"foo", "bar"},
		Replicas: 3,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	toSend := 2
	for i := 1; i <= toSend; i++ {
		msg := []byte(fmt.Sprintf("HELLO JSC-%d", i))
		if _, err = js.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}

	sl := c.streamLeader("$G", "TEST")

	fmt.Printf("\n\nSHUTDOWN STREAM LEADER %v\n\n", sl)

	sl.Shutdown()
	c.waitOnNewStreamLeader("$G", "TEST")

	fmt.Printf("\n\nSEND MORE MESSAGES\n\n")

	// Send 10 more while one replica offline.
	for i := toSend; i <= toSend*2; i++ {
		msg := []byte(fmt.Sprintf("HELLO JSC-%d", i))
		if _, err = js.Publish("foo", msg); err != nil {
			t.Fatalf("Unexpected publish error: %v", err)
		}
	}

	fmt.Printf("\n\nDELETING MSG %v\n\n", toSend)

	// Delete the first from the second batch.
	// Now delete a msg.
	dreq := server.JSApiMsgDeleteRequest{Seq: uint64(toSend)}
	dreqj, err := json.Marshal(dreq)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	resp, _ := nc.Request(fmt.Sprintf(server.JSApiMsgDeleteT, "TEST"), dreqj, time.Second)
	var delMsgResp server.JSApiMsgDeleteResponse
	if err = json.Unmarshal(resp.Data, &delMsgResp); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !delMsgResp.Success || delMsgResp.Error != nil {
		t.Fatalf("Got a bad response %+v", delMsgResp.Error)
	}

	fmt.Printf("\n\nRESTART OLD STREAM LEADER %v\n\n", sl)

	sl = c.restartServer(sl)
	c.checkClusterFormed()

	fmt.Printf("\n\nWAIT TO CATCHUP %v\n\n", sl)

	c.waitOnServerCurrent(sl)
}

func (c *cluster) restartServer(rs *server.Server) *server.Server {
	c.t.Helper()
	index := -1
	var opts *server.Options
	for i, s := range c.servers {
		if s == rs {
			index = i
			break
		}
	}
	if index < 0 {
		c.t.Fatalf("Could not find server %v to restart", rs)
	}
	opts = c.opts[index]
	s, o := RunServerWithConfig(opts.ConfigFile)
	if doLog {
		pre := fmt.Sprintf("[%s] - ", s.Name())
		s.SetLogger(logger.NewTestLogger(pre, true), true, true)
	}
	c.servers[index] = s
	c.opts[index] = o
	return s
}

func (c *cluster) checkClusterFormed() {
	checkClusterFormed(c.t, c.servers...)
}

func (c *cluster) waitOnPeerCount(n int) {
	c.t.Helper()
	c.waitOnLeader()
	leader := c.leader()
	expires := time.Now().Add(5 * time.Second)
	for time.Now().Before(expires) {
		peers := leader.JetStreamClusterPeers()
		if len(peers) == n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("Expected a cluster peer count of %d, got %d", n, len(leader.JetStreamClusterPeers()))
}

func (c *cluster) waitOnNewConsumerLeader(account, stream, consumer string) {
	c.t.Helper()
	expires := time.Now().Add(5 * time.Second)
	for time.Now().Before(expires) {
		if leader := c.consumerLeader(account, stream, consumer); leader != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("Expected a consumer leader for %q %q %q, got none", account, stream, consumer)
}

func (c *cluster) consumerLeader(account, stream, consumer string) *server.Server {
	c.t.Helper()
	for _, s := range c.servers {
		if s.JetStreamIsConsumerLeader(account, stream, consumer) {
			return s
		}
	}
	return nil
}

func (c *cluster) waitOnNewStreamLeader(account, stream string) {
	c.t.Helper()
	expires := time.Now().Add(5 * time.Second)
	for time.Now().Before(expires) {
		if leader := c.streamLeader(account, stream); leader != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("Expected a stream leader for %q %q, got none", account, stream)
}

func (c *cluster) streamLeader(account, stream string) *server.Server {
	c.t.Helper()
	for _, s := range c.servers {
		if s.JetStreamIsStreamLeader(account, stream) {
			return s
		}
	}
	return nil
}

func (c *cluster) waitOnServerCurrent(s *server.Server) {
	c.t.Helper()
	expires := time.Now().Add(5 * time.Second)
	for time.Now().Before(expires) {
		if s.JetStreamIsCurrent() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("Expected server %q to eventually be current", s)
}

func (c *cluster) randomNonLeader() *server.Server {
	// range should randomize.. but..
	for _, s := range c.servers {
		if !s.JetStreamIsLeader() {
			return s
		}
	}
	return nil
}

func (c *cluster) leader() *server.Server {
	for _, s := range c.servers {
		if s.JetStreamIsLeader() {
			return s
		}
	}
	return nil
}

// This needs to match raft.go:minElectionTimeout*2
const maxElectionTimeout = 550 * time.Millisecond

func (c *cluster) expectNoLeader() {
	c.t.Helper()
	expires := time.Now().Add(maxElectionTimeout)
	for time.Now().Before(expires) {
		if c.leader() != nil {
			c.t.Fatalf("Expected no leader but have one")
		}
	}
}

func (c *cluster) waitOnLeader() {
	c.t.Helper()
	expires := time.Now().Add(5 * time.Second)
	for time.Now().Before(expires) {
		if leader := c.leader(); leader != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("Expected a cluster leader, got none")
}

// Helper function to check that a cluster is formed
func (c *cluster) waitOnClusterReady() {
	c.t.Helper()
	var leader *server.Server
	expires := time.Now().Add(5 * time.Second)
	for time.Now().Before(expires) {
		if leader = c.leader(); leader != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Now make sure we have all peers.
	for leader != nil && time.Now().Before(expires) {
		fmt.Printf("len servers is %d, peers is %d\n", len(c.servers), len(leader.JetStreamClusterPeers()))
		if len(leader.JetStreamClusterPeers()) == len(c.servers) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("Expected a cluster leader and fully formed cluster")
}