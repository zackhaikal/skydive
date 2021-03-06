/*
 * Copyright (C) 2015 Red Hat, Inc.
 *
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 *
 */

package tests

import (
	"errors"
	"flag"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	shttp "github.com/redhat-cip/skydive/http"
	"github.com/redhat-cip/skydive/logging"
	"github.com/redhat-cip/skydive/tests/helper"
	"github.com/redhat-cip/skydive/topology/graph"
)

const confTopology = `---
ws_pong_timeout: 5

agent:
  listen: 58081
  topology:
    probes:
      - netlink
      - netns
      - ovsdb
      - docker

cache:
  expire: 300
  cleanup: 30

sflow:
  listen: 55000

ovs:
  ovsdb: 6400

etcd:
  embedded: true
  port: 2374
  data_dir: /tmp
  servers: http://localhost:2374

logging:
  default: {{.LogLevel}}
`

var graphBackend string

func init() {
	flag.StringVar(&graphBackend, "graph.backend", "memory", "Specify the graph backend used")
	flag.Parse()
}

func newClient() (*websocket.Conn, error) {
	conn, err := net.Dial("tcp", "127.0.0.1:58081")
	if err != nil {
		return nil, err
	}

	endpoint := "ws://127.0.0.1:58081/ws"
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}

	wsConn, _, err := websocket.NewClient(conn, u, http.Header{"Origin": {endpoint}}, 1024, 1024)
	if err != nil {
		return nil, err
	}

	return wsConn, nil
}

func connectToAgent(timeout int, onReady func(*websocket.Conn)) (*websocket.Conn, error) {
	var ws *websocket.Conn
	var err error

	t := 0
	for {
		if t > timeout {
			return nil, errors.New("Connection to Agent : timeout reached")
		}

		ws, err = newClient()
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
		t++
	}

	ready := false
	h := func(message string) error {
		err := ws.WriteControl(websocket.PongMessage, []byte(message), time.Now().Add(time.Second))
		if err != nil {
			return err
		}
		if !ready {
			ready = true
			onReady(ws)
		}
		return nil
	}
	ws.SetPingHandler(h)

	return ws, nil
}

func processGraphMessage(g *graph.Graph, m []byte) error {
	g.Lock()
	defer g.Unlock()

	msg, err := shttp.UnmarshalWSMessage(m)
	if err != nil {
		return err
	}

	if msg.Namespace != "Graph" {
		return nil
	}

	msg, err = graph.UnmarshalWSMessage(msg)
	if err != nil {
		return err
	}

	switch msg.Type {
	case "NodeUpdated":
		n := msg.Obj.(*graph.Node)
		node := g.GetNode(n.ID)
		if node != nil {
			g.SetMetadata(node, n.Metadata())
		}
	case "NodeDeleted":
		g.DelNode(msg.Obj.(*graph.Node))
	case "NodeAdded":
		n := msg.Obj.(*graph.Node)
		if g.GetNode(n.ID) == nil {
			g.AddNode(n)
		}
	case "EdgeUpdated":
		e := msg.Obj.(*graph.Edge)
		edge := g.GetEdge(e.ID)
		if edge != nil {
			g.SetMetadata(edge, e.Metadata())
		}
	case "EdgeDeleted":
		g.DelEdge(msg.Obj.(*graph.Edge))
	case "EdgeAdded":
		e := msg.Obj.(*graph.Edge)
		if g.GetEdge(e.ID) == nil {
			g.AddEdge(e)
		}
	}

	return nil
}

func startTopologyClient(t *testing.T, g *graph.Graph, onReady func(*websocket.Conn), onChange func(*websocket.Conn)) error {
	// ready when got a first ping
	ws, err := connectToAgent(5, onReady)
	if err != nil {
		return err
	}

	for {
		_, m, err := ws.ReadMessage()
		if err != nil {
			break
		}

		err = processGraphMessage(g, m)
		if err != nil {
			return err
		}

		logging.GetLogger().Debugf("%s", string(m))
		logging.GetLogger().Debugf("%s", g.String())

		onChange(ws)
	}

	return nil
}

func testTopology(t *testing.T, g *graph.Graph, cmds []helper.Cmd, onChange func(ws *websocket.Conn)) {
	cmdIndex := 0
	cmdChan := make(chan helper.Cmd, len(cmds))
	defer close(cmdChan)

	go func() {
		for cmd := range cmdChan {
			helper.ExecCmds(t, cmd)
		}
	}()

	or := func(w *websocket.Conn) {
		// ready to exec the first cmd
		if cmdIndex < len(cmds) {
			cmdChan <- cmds[cmdIndex]
			cmdIndex++
		}
	}

	oc := func(ws *websocket.Conn) {
		onChange(ws)

		// exec the following command
		if cmdIndex < len(cmds) {
			cmdChan <- cmds[cmdIndex]
			cmdIndex++
		}
	}

	err := startTopologyClient(t, g, or, oc)
	if err != nil {
		t.Fatal(err.Error())
	}
}

func testCleanup(t *testing.T, g *graph.Graph, cmds []helper.Cmd, names []string) {
	// cleanup side on the test
	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed {
			clean := true
			for _, name := range names {
				n := g.LookupFirstNode(graph.Metadata{"Name": name})
				if n != nil {
					clean = false
					break
				}
			}

			if clean {
				testPassed = true

				ws.Close()
			}
		}
	}

	testTopology(t, g, cmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}
}

func newGraph(t *testing.T) *graph.Graph {
	var backend graph.GraphBackend
	var err error
	switch graphBackend {
	case "gremlin-ws":
		backend, err = graph.NewGremlinBackend("ws://127.0.0.1:8182")
	case "gremlin-rest":
		backend, err = graph.NewGremlinBackend("http://127.0.0.1:8182?gremlin=")
	default:
		backend, err = graph.NewMemoryBackend()
	}

	if err != nil {
		t.Fatal(err.Error())
	}

	t.Logf("Using %s as backend", graphBackend)

	g, err := graph.NewGraph(backend)
	if err != nil {
		t.Fatal(err.Error())
	}

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err.Error())
	}

	root := g.LookupFirstNode(graph.Metadata{"Name": hostname, "Type": "host"})
	if root == nil {
		root = g.NewNode(graph.Identifier(hostname), graph.Metadata{"Name": hostname, "Type": "host"})
		if root == nil {
			t.Fatal("fail while adding root node")
		}
	}

	return g
}

func TestBridgeOVS(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"ovs-vsctl add-br br-test1", true},
	}

	tearDownCmds := []helper.Cmd{
		{"ovs-vsctl del-br br-test1", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 3 && len(g.GetEdges()) >= 2 {
			ovsbridge := g.LookupFirstNode(graph.Metadata{"Type": "ovsbridge", "Name": "br-test1"})
			if ovsbridge == nil {
				return
			}
			ovsports := g.LookupChildren(ovsbridge, graph.Metadata{"Type": "ovsport"})
			if len(ovsports) != 1 {
				return
			}
			devices := g.LookupChildren(ovsports[0], graph.Metadata{"Type": "internal", "Driver": "openvswitch"})
			if len(devices) != 1 {
				return
			}

			if ovsbridge.Metadata()["Host"] == "" || ovsports[0].Metadata()["Host"] == "" || devices[0].Metadata()["Host"] == "" {
				return
			}

			testPassed = true

			ws.Close()
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"br-test1"})
}

func TestPatchOVS(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"ovs-vsctl add-br br-test1", true},
		{"ovs-vsctl add-br br-test2", true},
		{"ovs-vsctl add-port br-test1 patch-br-test2 -- set interface patch-br-test2 type=patch", true},
		{"ovs-vsctl add-port br-test2 patch-br-test1 -- set interface patch-br-test1 type=patch", true},
		{"ovs-vsctl set interface patch-br-test2 option:peer=patch-br-test1", true},
		{"ovs-vsctl set interface patch-br-test1 option:peer=patch-br-test2", true},
	}

	tearDownCmds := []helper.Cmd{
		{"ovs-vsctl del-br br-test1", true},
		{"ovs-vsctl del-br br-test2", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 10 && len(g.GetEdges()) >= 9 {
			patch1 := g.LookupFirstNode(graph.Metadata{"Type": "patch", "Name": "patch-br-test1", "Driver": "openvswitch"})
			if patch1 == nil {
				return
			}

			patch2 := g.LookupFirstNode(graph.Metadata{"Type": "patch", "Name": "patch-br-test2", "Driver": "openvswitch"})
			if patch2 == nil {
				return
			}

			if !g.AreLinked(patch1, patch2) {
				return
			}

			testPassed = true

			ws.Close()
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"br-test1", "br-test2", "patch-br-test1", "patch-br-test2"})
}

func TestInterfaceOVS(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"ovs-vsctl add-br br-test1", true},
		{"ovs-vsctl add-port br-test1 intf1 -- set interface intf1 type=internal", true},
	}

	tearDownCmds := []helper.Cmd{
		{"ovs-vsctl del-br br-test1", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 5 && len(g.GetEdges()) >= 4 {
			intf := g.LookupFirstNode(graph.Metadata{"Type": "internal", "Name": "intf1", "Driver": "openvswitch"})
			if intf != nil {
				if _, ok := intf.Metadata()["UUID"]; ok {
					// check we don't have another interface potentially added by netlink
					// should only have ovsport and interface
					others := g.LookupNodes(graph.Metadata{"Name": "intf1"})
					if len(others) > 2 {
						return
					}

					if _, ok := intf.Metadata()["MAC"]; !ok {
						return
					}

					testPassed = true

					ws.Close()
				}
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"br-test1", "intf1"})
}

func TestBondOVS(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"ovs-vsctl add-br br-test1", true},
		{"ip tuntap add mode tap dev intf1", true},
		{"ip tuntap add mode tap dev intf2", true},
		{"ovs-vsctl add-bond br-test1 bond0 intf1 intf2", true},
	}

	tearDownCmds := []helper.Cmd{
		{"ovs-vsctl del-br br-test1", true},
		{"ip link del intf1", true},
		{"ip link del intf2", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 6 && len(g.GetEdges()) >= 5 {
			bond := g.LookupFirstNode(graph.Metadata{"Type": "ovsport", "Name": "bond0"})
			if bond != nil {
				intfs := g.LookupChildren(bond, nil)
				if len(intfs) != 2 {
					return
				}

				testPassed = true

				ws.Close()
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"br-test1", "intf1", "intf2"})
}

func TestVeth(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"ip l add vm1-veth0 type veth peer name vm1-veth1", true},
	}

	tearDownCmds := []helper.Cmd{
		{"ip link del vm1-veth0", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 2 && len(g.GetEdges()) >= 1 {
			veth0 := g.LookupFirstNode(graph.Metadata{"Type": "veth", "Name": "vm1-veth0"})
			if veth0 == nil {
				return
			}
			veth1 := g.LookupFirstNode(graph.Metadata{"Type": "veth", "Name": "vm1-veth1"})
			if veth1 == nil {
				return
			}

			if g.AreLinked(veth0, veth1) {
				testPassed = true

				ws.Close()
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"vm1-veth0", "vm1-veth1"})
}

func TestBridge(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"brctl addbr br-test", true},
		{"ip tuntap add mode tap dev intf1", true},
		{"brctl addif br-test intf1", true},
	}

	tearDownCmds := []helper.Cmd{
		{"brctl delbr br-test", true},
		{"ip link del intf1", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 2 && len(g.GetEdges()) >= 1 {
			bridge := g.LookupFirstNode(graph.Metadata{"Type": "bridge", "Name": "br-test"})
			if bridge != nil {
				nodes := g.LookupChildren(bridge, graph.Metadata{"Name": "intf1"})
				if len(nodes) == 1 {
					testPassed = true

					ws.Close()
				}
			}

		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"br-test", "intf1"})
}

func TestMacNameUpdate(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"ip l add vm1-veth0 type veth peer name vm1-veth1", true},
		{"ip l set vm1-veth1 name vm1-veth2", true},
		{"ip l set vm1-veth2 address 00:00:00:00:00:aa", true},
	}

	tearDownCmds := []helper.Cmd{
		{"ip link del vm1-veth0", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 2 && len(g.GetEdges()) >= 1 {
			node := g.LookupFirstNode(graph.Metadata{"Name": "vm1-veth2"})
			if node == nil {
				return
			}
			if mac, ok := node.Metadata()["MAC"]; ok && mac == "00:00:00:00:00:aa" {
				if g.LookupFirstNode(graph.Metadata{"Name": "vm1-veth1"}) == nil {
					testPassed = true

					ws.Close()
				}
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"vm1-veth0", "vm1-veth1", "vm1-veth2"})
}

func TestNameSpace(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"ip netns add ns1", true},
	}

	tearDownCmds := []helper.Cmd{
		{"ip netns del ns1", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 1 && len(g.GetEdges()) >= 1 {
			node := g.LookupFirstNode(graph.Metadata{"Name": "ns1", "Type": "netns"})
			if node != nil {
				testPassed = true

				ws.Close()
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"ns1"})
}

func TestNameSpaceVeth(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"ip netns add ns1", true},
		{"ip l add vm1-veth0 type veth peer name vm1-veth1 netns ns1", true},
	}

	tearDownCmds := []helper.Cmd{
		{"ip link del vm1-veth0", true},
		{"ip netns del ns1", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 1 && len(g.GetEdges()) >= 1 {
			node := g.LookupFirstNode(graph.Metadata{"Name": "ns1", "Type": "netns"})
			if node == nil {
				return
			}

			veth := g.LookupFirstChild(node, graph.Metadata{"Name": "vm1-veth1", "Type": "veth"})
			if veth != nil {
				testPassed = true

				ws.Close()
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"ns1", "vm1-veth0"})
}

func TestNameSpaceOVSInterface(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"ip netns add ns1", true},
		{"ovs-vsctl add-br br-test1", true},
		{"ovs-vsctl add-port br-test1 intf1 -- set interface intf1 type=internal", true},
		{"ip l set intf1 netns ns1", true},
	}

	tearDownCmds := []helper.Cmd{
		{"ovs-vsctl del-br br-test1", true},
		{"ip netns del ns1", true},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 2 && len(g.GetEdges()) >= 2 {
			node := g.LookupFirstNode(graph.Metadata{"Name": "ns1", "Type": "netns"})
			if node == nil {
				return
			}

			veth := g.LookupFirstChild(node, graph.Metadata{"Name": "intf1"})
			if veth == nil {
				return
			}

			children := g.LookupNodes(graph.Metadata{"Name": "intf1", "Type": "internal"})
			if len(children) == 1 {
				testPassed = true

				ws.Close()
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"ns1", "br-test1"})
}

func TestDockerSimple(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"docker run -d -t -i --name test-skydive-docker busybox", false},
	}

	tearDownCmds := []helper.Cmd{
		{"docker rm -f test-skydive-docker", false},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 1 && len(g.GetEdges()) >= 1 {
			if node := g.LookupFirstNode(graph.Metadata{"Name": "test-skydive-docker", "Type": "netns", "Manager": "docker"}); node != nil {
				if node := g.LookupFirstChild(node, graph.Metadata{"Type": "container", "Docker.ContainerName": "/test-skydive-docker"}); node != nil {
					testPassed = true
					ws.Close()
				}
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"test-skydive-docker"})
}

func TestDockerShareNamespace(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"docker run -d -t -i --name test-skydive-docker busybox", false},
		{"docker run -d -t -i --name test-skydive-docker2 --net=container:test-skydive-docker busybox", false},
	}

	tearDownCmds := []helper.Cmd{
		{"docker rm -f test-skydive-docker", false},
		{"docker rm -f test-skydive-docker2", false},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 1 && len(g.GetEdges()) >= 1 {
			nsNodes := g.LookupNodes(graph.Metadata{"Type": "netns", "Manager": "docker"})
			if len(nsNodes) > 1 {
				t.Error("There should be only one namespace managed by Docker")
				ws.Close()
			} else if len(nsNodes) == 1 {
				if node := g.LookupFirstChild(nsNodes[0], graph.Metadata{"Type": "container", "Docker.ContainerName": "/test-skydive-docker"}); node != nil {
					if node := g.LookupFirstChild(nsNodes[0], graph.Metadata{"Type": "container", "Docker.ContainerName": "/test-skydive-docker2"}); node != nil {
						testPassed = true
						ws.Close()
					}
				}
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"test-skydive-docker"})
}

func TestDockerNetHost(t *testing.T) {
	g := newGraph(t)

	agent := helper.StartAgentWithConfig(t, confTopology)
	defer agent.Stop()

	setupCmds := []helper.Cmd{
		{"docker run -d -t -i --net=host --name test-skydive-docker busybox", false},
	}

	tearDownCmds := []helper.Cmd{
		{"docker rm -f test-skydive-docker", false},
	}

	testPassed := false
	onChange := func(ws *websocket.Conn) {
		g.Lock()
		defer g.Unlock()

		if !testPassed && len(g.GetNodes()) >= 1 && len(g.GetEdges()) >= 1 {
			if node := g.LookupFirstNode(graph.Metadata{"Docker.ContainerName": "/test-skydive-docker", "Type": "container"}); node != nil {
				if node := g.LookupFirstNode(graph.Metadata{"Type": "netns", "Manager": "docker", "Name": "test-skydive-docker"}); node != nil {
					t.Error("There should be no netns node for container test-skydive-docker")
				} else {
					testPassed = true
				}
				ws.Close()
			}
		}
	}

	testTopology(t, g, setupCmds, onChange)
	if !testPassed {
		t.Error("test not executed or failed")
	}

	testCleanup(t, g, tearDownCmds, []string{"test-skydive-docker"})
}
