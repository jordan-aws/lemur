/**

Parallel Data Mover is scalable system to copy or migrate data between
various storage systems. It supports multliple types of sources and
destinations, including POSIX, S3, HPSS, etc.

Use cases include:
  * Data movement for Lustre HSM.
  * Offsite replication for DR
  * Lustre file-level replication
  * Storage rebalancing within a single tier
  * Migration between filesytems (e.g GPFS - > Lustre)

Initially the main focus is for HSM.
*/

package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/golang/glog"
	"github.intel.com/hpdd/lustre/fs"
	"github.intel.com/hpdd/lustre/hsm"
	"github.intel.com/hpdd/lustre/llapi"
	"github.intel.com/hpdd/policy/pdm"
	"github.intel.com/hpdd/policy/pkg/workq"
)

type (
	// CopyTool for a single filesytem and a collection of backends.
	CopyTool struct {
		root  fs.RootDir
		agent hsm.Agent
		queue *workq.Master
		wg    sync.WaitGroup
	}
)

func (ct *CopyTool) Stop() {
	if ct.agent != nil {
		ct.agent.Stop()
	}
}

func (ct *CopyTool) initAgent(done chan struct{}) error {
	var err error
	ct.agent, err = hsm.Start(ct.root, done)

	if err != nil {
		return err
	}

	return nil
}

func hsm2pdmCommand(a llapi.HsmAction) (c pdm.CommandType) {
	switch a {
	case llapi.HsmActionArchive:
		c = pdm.ArchiveCommand
	case llapi.HsmActionRestore:
		c = pdm.RestoreCommand
	case llapi.HsmActionRemove:
		c = pdm.RemoveCommand
	case llapi.HsmActionCancel:
		c = pdm.CancelCommand
	default:
		log.Fatalf("unknown command: %v", a)
	}

	return

}

func (ct *CopyTool) handleActions() {
	ch := ct.agent.Actions()
	for ai := range ch {
		log.Printf("incoming: %s", ai)
		aih, err := ai.Begin(0, false)
		if err != nil {
			log.Printf("begin failed: %v", err)
			continue
		}

		req := &pdm.Request{
			Agent:      "me",
			Cookie:     aih.Cookie(),
			SourcePath: fs.FidPath(ct.root, aih.Fid()),
			Endpoint:   "posix",
			Command:    hsm2pdmCommand(aih.Action()),
			Archive:    aih.ArchiveID(),
			Offset:     aih.Offset(),
			Length:     aih.Length(),
			Params:     "",
		}
		log.Printf("Request: %#v", req)
		ct.queue.Put(req)

		reply := &pdm.Result{}
		o, err := ct.queue.Wait()
		if err != nil {
			log.Printf("queue error: %v\n", err)
		}
		err = o.GetData(reply)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("reply: %v\n", reply)
		// XXX for develoment, prevent coordinator constipation
		log.Printf("end: %s", ai)
		aih.End(0, 0, 0, -1)
	}
}

func (ct *CopyTool) addHandler() {
	ct.wg.Add(1)
	go func() {
		defer ct.wg.Done()
		ct.handleActions()
	}()
}

func agent(conf *pdm.HSMConfig) {
	root, err := fs.MountRoot(conf.Lustre)
	if err != nil {
		glog.Fatal(err)
	}

	done := make(chan struct{})
	ct := &CopyTool{
		root:  root,
		queue: workq.NewMaster("pdm", conf.RedisServer),
	}

	interruptHandler(func() {
		close(done)
		ct.Stop()
	})

	go func() {
		err := ct.initAgent(done)
		if err != nil {
			log.Fatal(err)
			return
		}

		for i := 0; i < conf.Processes; i++ {
			ct.addHandler()
		}
	}()

	<-done
	ct.wg.Wait()

}

var (
	reset = false
	trace = false
)

func init() {
	flag.BoolVar(&reset, "reset", false, "Reset queue")
	flag.BoolVar(&trace, "trace", false, "Print redis trace")
}

func main() {
	flag.Parse()
	// server := ":6379"
	//	password := ""

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	//	if reset {
	//		mq.RedisMQReset(pool, queueName)
	//	}

	defer glog.Flush()

	conf := pdm.ConfigInitMust()
	glog.V(2).Infof("current configuration:\n%v", conf.String())

	agent(conf)
}

func interruptHandler(once func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM)

	go func() {
		stopping := false
		for sig := range c {
			glog.Infoln("signal received:", sig)
			if !stopping {
				stopping = true
				once()
			}
		}
	}()

}
