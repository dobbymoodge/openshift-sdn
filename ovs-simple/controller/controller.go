package controller

import (
	"crypto/md5"
	"errors"
	"fmt"
	log "github.com/golang/glog"
	"net"
	"os/exec"
	"strconv"
	"time"

	"github.com/openshift/openshift-sdn/pkg/netutils"
	"github.com/openshift/openshift-sdn/pkg/registry"
)

const (
	ContainerNetwork      string = "10.1.0.0/16"
	ContainerSubnetLength uint   = 8
)

type Controller interface {
	StartMaster(sync bool) error
	StartNode(sync, skipsetup bool) error
	AddNode(minionIP string) error
	DeleteNode(minionIP string) error
	Stop()
}

type OvsController struct {
	sm       registry.SubnetRegistry
	localIP  string
	localSub *registry.Subnet
	hostName string
	sna      *netutils.SubnetAllocator
	sig      chan struct{}
}

func NewController(sub registry.SubnetRegistry, hostname string, selfip string) Controller {
	if selfip == "" {
		addrs, err := net.LookupIP(hostname)
		if err != nil {
			log.Errorf("Failed to lookup IP Address for %s", hostname)
			return nil
		}
		selfip = addrs[0].String()
	}
	log.Infof("Self IP : %s", selfip)
	return &OvsController{
		sm:       sub,
		localIP:  selfip,
		hostName: hostname,
		localSub: nil,
		sna:      nil,
		sig:      make(chan struct{}),
	}
}

func (oc *OvsController) StartMaster(sync bool) error {
	// wait a minute for etcd to come alive
	status := oc.sm.CheckEtcdIsAlive(60)
	if !status {
		log.Errorf("Etcd not running?")
		return errors.New("Etcd not reachable. Sync cluster check failed.")
	}
	// initialize the minion key
	if sync {
		err := oc.sm.InitMinions()
		if err != nil {
			log.Infof("Minion path already initialized.")
		}
	}

	// initialize the subnet key?
	err := oc.sm.InitSubnets()
	subrange := make([]string, 0)
	if err != nil {
		subnets, err := oc.sm.GetSubnets()
		if err != nil {
			log.Errorf("Error in initializing/fetching subnets - %v\n", err)
			return err
		}
		for _, sub := range *subnets {
			subrange = append(subrange, sub.Sub)
		}
	}
	oc.sna, _ = netutils.NewSubnetAllocator(ContainerNetwork, ContainerSubnetLength, subrange)
	err = oc.ServeExistingMinions()
	if err != nil {
		log.Warningf("Error initializing existing minions. %v.", err)
		// no worry, we can still keep watching it.
	}
	go oc.watchMinions()
	return nil
}

func (oc *OvsController) ServeExistingMinions() error {
	minions, err := oc.sm.GetMinions()
	if err != nil {
		return err
	}

	for _, minion := range *minions {
		_, err := oc.sm.GetSubnet(minion)
		if err == nil {
			// subnet already exists, continue
			continue
		}
		err = oc.AddNode(minion)
		if err != nil {
			return err
		}
	}
	return nil
}

func (oc *OvsController) AddNode(minion string) error {
	sn, err := oc.sna.GetNetwork()
	if err != nil {
		log.Errorf("Error creating network for minion - %s\n", minion)
		return err
	}
	addrs, err := net.LookupIP(minion)
	if err != nil {
		log.Errorf("Failed to lookup IP Address for %s", minion)
		return err
	}
	minionIP := addrs[0].String()
	if minionIP == "" {
		// minion's name is the IP address itself
		minionIP = minion
	}
	sub := &registry.Subnet{
		Minion: minionIP,
		Sub:    sn.String(),
	}
	oc.sm.CreateSubnet(minion, sub)
	if err != nil {
		log.Errorf("Error writing subnet to etcd for minion %s - %v\n", minion, sn)
		return err
	}
	return nil
}

func (oc *OvsController) DeleteNode(minion string) error {
	sub, err := oc.sm.GetSubnet(minion)
	if err != nil {
		log.Errorf("Error fetching minion subnet for %s for delete operation.", minion)
		return err
	}
	_, ipnet, err := net.ParseCIDR(sub.Sub)
	if err != nil {
		log.Errorf("Error parsing subnet for minion %s, for deletion.", sub.Sub)
		return err
	}
	oc.sna.ReleaseNetwork(ipnet)
	return oc.sm.DeleteSubnet(minion)
}

func (oc *OvsController) syncWithMaster() error {
	return oc.sm.CreateMinion(oc.hostName, oc.localIP)
}

func (oc *OvsController) StartNode(sync, skipsetup bool) error {
	if sync {
		err := oc.syncWithMaster()
		if err != nil {
			log.Errorf("Failed to register with master. (%s)", err.Error())
			return err
		}
	}
	err := oc.initSelfSubnet()
	if err != nil {
		log.Errorf("Failed to get subnet for this host. (%v)", err)
		return err
	}
	// restart docker daemon
	_, ipnet, err := net.ParseCIDR(oc.localSub.Sub)
	if err == nil {
		if !skipsetup {
			out, err := exec.Command("openshift-sdn-simple-setup-node.sh", netutils.GenerateDefaultGateway(ipnet).String(), ipnet.String(), ContainerNetwork).CombinedOutput()
			log.Infof("Output of setup script: %s", out)
			if err != nil {
				log.Errorf("Error executing setup script. \n\tOutput: %s\n\tError: %v\n", out, err)
				return err
			}
		}
		exec.Command("ovs-ofctl", "-O", "OpenFlow13", "del-flows", "br0").CombinedOutput()
		subnets, err := oc.sm.GetSubnets()
		if err != nil {
			log.Errorf("Could not fetch existing subnets. %v", err)
		}
		for _, s := range *subnets {
			oc.AddOFRules(s.Minion, s.Sub)
		}
		go oc.watchCluster()
	}
	return err
}

func (oc *OvsController) initSelfSubnet() error {
	// get subnet for self
	for {
		sub, err := oc.sm.GetSubnet(oc.hostName)
		if err != nil {
			log.Errorf("Could not find an allocated subnet for this minion (%s)(%s). Waiting..\n", oc.hostName, err)
			time.Sleep(2 * time.Second)
			continue
		}
		oc.localSub = sub
		return nil
	}
}

func (oc *OvsController) watchMinions() {
	// watch latest?
	stop := make(chan bool)
	minevent := make(chan *registry.MinionEvent)
	go oc.sm.WatchMinions(0, minevent, stop)
	for {
		select {
		case ev := <-minevent:
			switch ev.Type {
			case registry.Added:
				_, err := oc.sm.GetSubnet(ev.Minion)
				if err != nil {
					// subnet does not exist already
					oc.AddNode(ev.Minion)
				}
			case registry.Deleted:
				oc.DeleteNode(ev.Minion)
			}
		case <-oc.sig:
			log.Error("Signal received. Stopping watching of minions.")
			stop <- true
			return
		}
	}
}

func (oc *OvsController) watchCluster() {
	stop := make(chan bool)
	clusterEvent := make(chan *registry.SubnetEvent)
	go oc.sm.WatchSubnets(0, clusterEvent, stop)
	for {
		select {
		case ev := <-clusterEvent:
			switch ev.Type {
			case registry.Added:
				// add openflow rules
				oc.AddOFRules(ev.Sub.Minion, ev.Sub.Sub)
			case registry.Deleted:
				// delete openflow rules meant for the minion
				oc.DelOFRules(ev.Sub.Minion)
			}
		case <-oc.sig:
			stop <- true
			return
		}
	}
}

func (oc *OvsController) Stop() {
	close(oc.sig)
	//oc.sig <- struct{}{}
}

func (oc *OvsController) AddOFRules(minionip, subnet string) {
	cookie := generateCookie(minionip)
	if minionip == oc.localIP {
		// self, so add the input rules
		iprule := fmt.Sprintf("table=0,cookie=0x%s,priority=200,ip,in_port=10,nw_dst=%s,actions=output:9", cookie, subnet)
		arprule := fmt.Sprintf("table=0,cookie=0x%s,priority=200,arp,in_port=10,nw_dst=%s,actions=output:9", cookie, subnet)
		o, e := exec.Command("ovs-ofctl", "-O", "OpenFlow13", "add-flow", "br0", iprule).CombinedOutput()
		log.Infof("Output of adding %s: %s (%v)", iprule, o, e)
		o, e = exec.Command("ovs-ofctl", "-O", "OpenFlow13", "add-flow", "br0", arprule).CombinedOutput()
		log.Infof("Output of adding %s: %s (%v)", arprule, o, e)
	} else {
		iprule := fmt.Sprintf("table=0,cookie=0x%s,priority=200,ip,in_port=9,nw_dst=%s,actions=set_field:%s->tun_dst,output:10", cookie, subnet, minionip)
		arprule := fmt.Sprintf("table=0,cookie=0x%s,priority=200,arp,in_port=9,nw_dst=%s,actions=set_field:%s->tun_dst,output:10", cookie, subnet, minionip)
		o, e := exec.Command("ovs-ofctl", "-O", "OpenFlow13", "add-flow", "br0", iprule).CombinedOutput()
		log.Infof("Output of adding %s: %s (%v)", iprule, o, e)
		o, e = exec.Command("ovs-ofctl", "-O", "OpenFlow13", "add-flow", "br0", arprule).CombinedOutput()
		log.Infof("Output of adding %s: %s (%v)", arprule, o, e)
	}
}

func (oc *OvsController) DelOFRules(minion string) {
	log.Infof("Calling del rules for %s", minion)
	cookie := generateCookie(minion)
	if minion == oc.localIP {
		iprule := fmt.Sprintf("table=0,cookie=0x%s/0xffffffff,ip,in_port=10", cookie)
		arprule := fmt.Sprintf("table=0,cookie=0x%s/0xffffffff,arp,in_port=10", cookie)
		o, e := exec.Command("ovs-ofctl", "-O", "OpenFlow13", "del-flows", "br0", iprule).CombinedOutput()
		log.Infof("Output of deleting local ip rules %s (%v)", o, e)
		o, e = exec.Command("ovs-ofctl", "-O", "OpenFlow13", "del-flows", "br0", arprule).CombinedOutput()
		log.Infof("Output of deleting local ip rules %s (%v)", o, e)
	} else {
		iprule := fmt.Sprintf("table=0,cookie=0x%s/0xffffffff,ip,in_port=9", cookie)
		arprule := fmt.Sprintf("table=0,cookie=0x%s/0xffffffff,arp,in_port=9", cookie)
		o, e := exec.Command("ovs-ofctl", "-O", "OpenFlow13", "del-flows", "br0", iprule).CombinedOutput()
		log.Infof("Output of deleting %s: %s (%v)", iprule, o, e)
		o, e = exec.Command("ovs-ofctl", "-O", "OpenFlow13", "del-flows", "br0", arprule).CombinedOutput()
		log.Infof("Output of deleting %s: %s (%v)", arprule, o, e)
	}
}

func generateCookie(ip string) string {
	return strconv.FormatInt(int64(md5.Sum([]byte(ip))[0]), 16)
}
