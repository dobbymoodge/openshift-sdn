package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	log "github.com/golang/glog"
	"github.com/openshift/openshift-sdn/ovs-simple/controller"
	"github.com/openshift/openshift-sdn/pkg/registry"
	"github.com/openshift/openshift-sdn/pkg/version"
)

type CmdLineOpts struct {
	etcdEndpoints string
	etcdPath      string
	etcdKeyfile   string
	etcdCertfile  string
	etcdCAFile    string
	ip            string
	hostname      string
	master        bool
	minion        bool
	skipsetup     bool
	sync          bool
	version       bool
	help          bool
}

var opts CmdLineOpts

func init() {
	flag.StringVar(&opts.etcdEndpoints, "etcd-endpoints", "http://127.0.0.1:4001", "a comma-delimited list of etcd endpoints")
	flag.StringVar(&opts.etcdPath, "etcd-path", "/registry/sdn/", "etcd path")
	flag.StringVar(&opts.etcdKeyfile, "etcd-keyfile", "", "SSL key file used to secure etcd communication")
	flag.StringVar(&opts.etcdCertfile, "etcd-certfile", "", "SSL certification file used to secure etcd communication")
	flag.StringVar(&opts.etcdCAFile, "etcd-cafile", "", "SSL Certificate Authority file used to secure etcd communication")

	flag.StringVar(&opts.ip, "public-ip", "", "Publicly reachable IP address of this host (for node mode).")
	flag.StringVar(&opts.hostname, "hostname", "", "Hostname as registered with master (for node mode), will default to 'hostname -f'")

	flag.BoolVar(&opts.master, "master", true, "Run in master mode")
	flag.BoolVar(&opts.minion, "minion", false, "Run in minion mode")
	flag.BoolVar(&opts.skipsetup, "skip-setup", false, "Skip the setup when in minion mode")
	flag.BoolVar(&opts.sync, "sync", false, "Sync the minions directly to etcd-path (Do not wait for PaaS to do so!)")

	flag.BoolVar(&opts.version, "version", false, "print version info")
	flag.BoolVar(&opts.help, "help", false, "print this message")
}

func newNetworkManager() controller.Controller {
	sub := newSubnetRegistry()
	fqdn := opts.hostname
	if fqdn == "" {
		fqdn_bytes, _ := exec.Command("hostname", "-f").CombinedOutput()
		fqdn = strings.TrimSpace(string(fqdn_bytes))
	}
	return controller.NewController(sub, string(fqdn), opts.ip)
}

func newSubnetRegistry() registry.SubnetRegistry {
	peers := strings.Split(opts.etcdEndpoints, ",")

	subnetPath := path.Join(opts.etcdPath + "subnets")
	minionPath := "/registry/minions/"
	if opts.sync {
		minionPath = path.Join(opts.etcdPath + "minions")
	}

	cfg := &registry.EtcdConfig{
		Endpoints:  peers,
		Keyfile:    opts.etcdKeyfile,
		Certfile:   opts.etcdCertfile,
		CAFile:     opts.etcdCAFile,
		SubnetPath: subnetPath,
		MinionPath: minionPath,
	}

	for {
		esr, err := registry.NewEtcdSubnetRegistry(cfg)
		if err == nil {
			return esr
		}

		log.Error("Failed to create SubnetRegistry: %v ", err)
		time.Sleep(time.Second)
	}
}

func main() {
	// glog will log to tmp files by default. override so all entries
	// can flow into journald (if running under systemd)
	flag.Set("logtostderr", "true")

	// now parse command line args
	flag.Parse()

	if opts.help {
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTION]...\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(0)
	}

	if opts.version {
		fmt.Fprintf(os.Stdout, "openshift-sdn %v\n", version.Get())
		os.Exit(0)
	}

	// Register for SIGINT and SIGTERM and wait for one of them to arrive
	log.Info("Installing signal handlers")
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	be := newNetworkManager()
	if opts.minion {
		err := be.StartNode(opts.sync, opts.skipsetup)
		if err != nil {
			return
		}
	} else if opts.master {
		err := be.StartMaster(opts.sync)
		if err != nil {
			return
		}
	}

	select {
	case <-sigs:
		// unregister to get default OS nuke behaviour in case we don't exit cleanly
		signal.Stop(sigs)

		log.Info("Exiting...")
		be.Stop()
	}
}
