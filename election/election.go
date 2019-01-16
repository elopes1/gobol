package election

import (
	"fmt"
	"os"
	"time"

	"github.com/samuel/go-zookeeper/zk"
	"go.uber.org/zap"
)

//
// A zookeeper cluster election manager
// author: rnojiri
//

const defaultChannelSize int = 5
const terminalChannelSize int = 2

// Manager - handles the zookeeper election
type Manager struct {
	zkConnection                  *zk.Conn
	config                        *Config
	isMaster                      bool
	defaultACL                    []zk.ACL
	logger                        *zap.Logger
	feedbackChannel               chan int
	clusterConnectionEventChannel <-chan zk.Event
	electionFlowChannel           chan int
	nodeFlowChannel               chan int
	terminateElectionChannel      chan bool
	sessionID                     int64
	nodeName                      string
	disconnectedEvent             zk.Event
	clusterNodes                  map[string]bool
}

// New - creates a new instance
func New(config *Config, logger *zap.Logger) (*Manager, error) {

	return &Manager{
		zkConnection:                  nil,
		config:                        config,
		defaultACL:                    zk.WorldACL(zk.PermAll),
		logger:                        logger,
		feedbackChannel:               make(chan int, defaultChannelSize),
		terminateElectionChannel:      make(chan bool, terminalChannelSize),
		clusterConnectionEventChannel: nil,
		electionFlowChannel:           nil,
		nodeFlowChannel:               nil,
		disconnectedEvent:             zk.Event{Type: EventDisconnected},
		clusterNodes:                  map[string]bool{},
	}, nil
}

// getNodeData - check if node exists
func (m *Manager) getNodeData(node string) (*string, error) {

	data, _, err := m.zkConnection.Get(node)

	exists := true
	if err != nil {
		if err.Error() == "zk: node does not exist" {
			exists = false
		} else {
			return nil, err
		}
	}

	if !exists {
		return nil, nil
	}

	result := string(data)

	return &result, nil
}

// getZKMasterNode - returns zk master node name
func (m *Manager) getZKMasterNode() (*string, error) {

	if m.zkConnection == nil {
		return nil, nil
	}

	data, err := m.getNodeData(m.config.ZKElectionNodeURI)
	if err != nil {
		m.logError("getZKMasterNode", "error retrieving ZK election node data")
		return nil, err
	}

	return data, nil
}

// connect - connects to the zookeeper
func (m *Manager) connect() error {

	m.logInfo("connect", "connecting to zookeeper...")

	var err error

	// Create the ZK connection
	m.zkConnection, m.clusterConnectionEventChannel, err = zk.Connect(m.config.ZKURL, time.Duration(m.config.SessionTimeout)*time.Second)
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case event := <-m.clusterConnectionEventChannel:
				if event.Type == zk.EventSession {
					if event.State == zk.StateConnected ||
						event.State == zk.StateConnectedReadOnly {
						m.logInfo("connect", "connection established with zookeeper")
					} else if event.State == zk.StateSaslAuthenticated ||
						event.State == zk.StateHasSession {
						m.logInfo("connect", "session created in zookeeper")
					} else if event.State == zk.StateAuthFailed ||
						event.State == zk.StateDisconnected ||
						event.State == zk.StateExpired {
						m.logInfo("connect", "zookeeper connection was lost")
						m.disconnect()
						m.electionFlowChannel <- Disconnected
						m.nodeFlowChannel <- Disconnected
						for {
							time.Sleep(time.Duration(m.config.ReconnectionTimeout) * time.Second)
							m.zkConnection, m.clusterConnectionEventChannel, err = zk.Connect(m.config.ZKURL, time.Duration(m.config.SessionTimeout)*time.Second)
							if err != nil {
								m.logError("connect", "error reconnecting to zookeeper: "+err.Error())
							} else {
								_, err := m.Start()
								if err != nil {
									m.logError("connect", "error starting election loop: "+err.Error())
								} else {
									break
								}
							}
						}
					}
				}
			case <-m.terminateElectionChannel:
				m.logInfo("connect", "terminating connection channel")
				return
			}
		}
	}()

	return nil
}

// Start - starts to listen zk events
func (m *Manager) Start() (*chan int, error) {

	err := m.connect()
	if err != nil {
		m.logError("Start", "error connecting to zookeeper: "+err.Error())
		return nil, err
	}

	err = m.electForMaster()
	if err != nil {
		m.logError("Start", "error electing this node for master: "+err.Error())
		return nil, err
	}

	err = m.createSlaveDir("Start")
	if err != nil {
		m.logError("Start", "error creating slave directory: "+err.Error())
		return nil, err
	}

	err = m.listenForElectionEvents()
	if err != nil {
		m.logError("Start", "error listening for zk election node events: "+err.Error())
		return nil, err
	}

	err = m.listenForNodeEvents()
	if err != nil {
		m.logError("Start", "error listening for zk slave node events: "+err.Error())
		return nil, err
	}

	return &m.feedbackChannel, nil
}

// listenForElectionEvents - starts to listen for election node events
func (m *Manager) listenForElectionEvents() error {

	_, _, electionEventsChannel, err := m.zkConnection.ExistsW(m.config.ZKElectionNodeURI)
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case event := <-electionEventsChannel:
				if event.Type == zk.EventNodeDeleted {
					m.logInfo("listenForElectionEvents", "master has quit, trying to be the new master...")
					err := m.electForMaster()
					if err != nil {
						m.logError("listenForElectionEvents", "error trying to elect this node for master: "+err.Error())
					}
				} else if event.Type == zk.EventNodeCreated {
					m.logInfo("listenForElectionEvents", "a new master has been elected...")
				}
			case event := <-m.electionFlowChannel:
				if event == Disconnected {
					m.logInfo("listenForElectionEvents", "breaking election loop...")
					m.isMaster = false
					m.feedbackChannel <- Disconnected
					return
				}
			}
		}
	}()

	return nil
}

// listenForNodeEvents - starts to listen for node events
// Note: the zkConnection.ExistsW(...) and zkConnection.ChildrenW(...) does not work in the expected way, so I'm doing this manually
func (m *Manager) listenForNodeEvents() error {

	cluster, err := m.GetClusterInfo()
	if err != nil {
		return err
	}

	for _, node := range cluster.Nodes {
		m.clusterNodes[node] = true
	}

	ticker := time.NewTicker(time.Duration(m.config.ClusterChangeCheckTime) * time.Millisecond)

	go func() {
		for {
			select {
			case <-ticker.C:
				cluster, err := m.GetClusterInfo()
				if err != nil {
					m.logError("listenForNodeEvents", err.Error())
				} else {
					changed := false
					if len(cluster.Nodes) != len(m.clusterNodes) {
						changed = true
					} else {
						for _, node := range cluster.Nodes {
							if _, ok := m.clusterNodes[node]; !ok {
								changed = true
								break
							}
						}
					}
					if changed {
						m.logInfo("listenForNodeEvents", "cluster node configuration changed")
						for k := range m.clusterNodes {
							delete(m.clusterNodes, k)
						}
						for _, node := range cluster.Nodes {
							m.clusterNodes[node] = true
						}
						m.feedbackChannel <- ClusterChanged
					}
				}
			case event := <-m.nodeFlowChannel:
				if event == Disconnected {
					ticker.Stop()
					m.logInfo("listenForNodeEvents", "breaking node events loop...")
					return
				}
			}
		}
	}()

	return nil
}

// disconnect - disconnects from the zookeeper
func (m *Manager) disconnect() {

	if m.zkConnection != nil && m.zkConnection.State() != zk.StateDisconnected {
		m.zkConnection.Close()
		time.Sleep(2 * time.Second)
		m.logInfo("Close", "ZK connection closed")
	} else {
		m.logInfo("Close", "ZK connection is already closed")
	}
}

// Terminate - end all channels and disconnects from the zookeeper
func (m *Manager) Terminate() {

	m.terminateElectionChannel <- true
	m.electionFlowChannel <- Disconnected
	m.nodeFlowChannel <- Disconnected
	m.disconnect()
}

// GetHostname - retrieves this node hostname from the OS
func (m *Manager) GetHostname() (string, error) {

	name, err := os.Hostname()
	if err != nil {
		m.logError("GetHostname", "could not retrive this node hostname: "+err.Error())
		return "", err
	}

	return name, nil
}

// createSlaveDir - creates the slave directory
func (m *Manager) createSlaveDir(funcName string) error {

	data, err := m.getNodeData(m.config.ZKSlaveNodesURI)
	if err != nil {
		return err
	}

	if data == nil {
		path, err := m.zkConnection.Create(m.config.ZKSlaveNodesURI, nil, int32(0), m.defaultACL)
		if err != nil {
			m.logError(funcName, "error creating slave node directory: "+err.Error())
			return err
		}
		m.logInfo(funcName, "slave node directory created: "+path)
	}

	return nil
}

// registerAsSlave - register this node as a slave
func (m *Manager) registerAsSlave(nodeName string) error {

	err := m.createSlaveDir("registerAsSlave")
	if err != nil {
		return err
	}

	slaveNode := m.config.ZKSlaveNodesURI + "/" + nodeName

	data, err := m.getNodeData(slaveNode)
	if err != nil {
		return err
	}

	if data == nil {
		path, err := m.zkConnection.Create(slaveNode, []byte(nodeName), int32(zk.FlagEphemeral), m.defaultACL)
		if err != nil {
			m.logError("registerAsSlave", "error creating a slave node: "+err.Error())
			return err
		}

		m.logInfo("registerAsSlave", "slave node created: "+path)
	} else {
		m.logInfo("registerAsSlave", "slave node already exists: "+slaveNode)
	}

	m.isMaster = false
	m.feedbackChannel <- Slave

	return nil
}

// electForMaster - try to elect this node as the master
func (m *Manager) electForMaster() error {

	name, err := m.GetHostname()
	if err != nil {
		return err
	}

	zkMasterNode, err := m.getZKMasterNode()
	if err != nil {
		return err
	}

	if zkMasterNode != nil {
		if name == *zkMasterNode {
			m.logInfo("electForMaster", "this node is the master: "+*zkMasterNode)
			m.isMaster = true
		} else {
			m.logInfo("electForMaster", "another node is the master: "+*zkMasterNode)
			return m.registerAsSlave(name)
		}
	}

	path, err := m.zkConnection.Create(m.config.ZKElectionNodeURI, []byte(name), int32(zk.FlagEphemeral), m.defaultACL)
	if err != nil {
		if err.Error() == "zk: node already exists" {
			m.logInfo("electForMaster", "some node has became master before this node")
			return m.registerAsSlave(name)
		}

		m.logError("electForMaster", "error creating node: "+err.Error())
		return err
	}

	m.logInfo("electForMaster", "master node created: "+path)
	m.isMaster = true
	m.feedbackChannel <- Master

	slaveNode := m.config.ZKSlaveNodesURI + "/" + name
	slave, err := m.getNodeData(slaveNode)
	if err != nil {
		m.logError("electForMaster", fmt.Sprintf("error retrieving a slave node data '%s': %s\n", slaveNode, err.Error()))
		return nil
	}

	if slave != nil {
		err = m.zkConnection.Delete(slaveNode, 0)
		if err != nil {
			m.logError("electForMaster", fmt.Sprintf("error deleting slave node '%s': %s\n", slaveNode, err.Error()))
		} else {
			m.logInfo("electForMaster", "slave node deleted: "+slaveNode)
		}
	}

	return nil
}

// IsMaster - check if the cluster is the master
func (m *Manager) IsMaster() bool {
	return m.isMaster
}

// GetClusterInfo - return cluster info
func (m *Manager) GetClusterInfo() (*Cluster, error) {

	if m.zkConnection == nil {
		return nil, nil
	}

	nodes := []string{}
	masterNode, err := m.getZKMasterNode()
	if err != nil {
		return nil, err
	}

	nodes = append(nodes, *masterNode)

	slaveDir, err := m.getNodeData(m.config.ZKSlaveNodesURI)
	if err != nil {
		return nil, err
	}

	var children []string
	if slaveDir != nil {
		children, _, err = m.zkConnection.Children(m.config.ZKSlaveNodesURI)
		if err != nil {
			m.logError("GetClusterInfo", "error getting slave nodes: "+err.Error())
			return nil, err
		}

		nodes = append(nodes, children...)
	} else {
		children = []string{}
	}

	return &Cluster{
		IsMaster: m.isMaster,
		Master:   *masterNode,
		Slaves:   children,
		Nodes:    nodes,
		NumNodes: len(nodes),
	}, nil
}