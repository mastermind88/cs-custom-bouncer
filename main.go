package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/coreos/go-systemd/daemon"
	log "github.com/sirupsen/logrus"

	"github.com/crowdsecurity/cs-custom-bouncer/pkg/version"
	csbouncer "github.com/crowdsecurity/go-cs-bouncer"
	"gopkg.in/tomb.v2"
)

const (
	name = "cs-custom-bouncer"
)

var t tomb.Tomb

func termHandler(sig os.Signal, custom *customBouncer) error {
	if err := custom.ShutDown(); err != nil {
		return err
	}
	return nil
}

func HandleSignals(custom *customBouncer) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan,
		syscall.SIGTERM)

	exitChan := make(chan int)
	go func() {
		for {
			s := <-signalChan
			switch s {
			// kill -SIGTERM XXXX
			case syscall.SIGTERM:
				if err := termHandler(s, custom); err != nil {
					log.Fatalf("shutdown fail: %s", err)
				}
				exitChan <- 0
			}
		}
	}()

	code := <-exitChan
	log.Infof("Shutting down custom-bouncer service")
	os.Exit(code)
}

func main() {
	var err error
	log.Infof("cs-custom-bouncer %s", version.VersionStr())
	configPath := flag.String("c", "", "path to cs-custom-bouncer.yaml")
	verbose := flag.Bool("v", false, "set verbose mode")

	flag.Parse()

	if configPath == nil || *configPath == "" {
		log.Fatalf("configuration file is required")
	}

	config, err := NewConfig(*configPath)
	if err != nil {
		log.Fatalf("unable to load configuration: %s", err)
	}

	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	custom, err := newCustomBouncer(config.BinPath)
	if err != nil {
		log.Fatalf(err.Error())
	}

	if err := custom.Init(); err != nil {
		log.Fatalf(err.Error())
	}

	bouncer := &csbouncer.StreamBouncer{
		APIKey:         config.APIKey,
		APIUrl:         config.APIUrl,
		TickerInterval: config.UpdateFrequency,
		UserAgent:      fmt.Sprintf("%s/%s", name, version.VersionStr()),
	}
	if err := bouncer.Init(); err != nil {
		log.Fatalf(err.Error())
	}

	go bouncer.Run()

	t.Go(func() error {
		log.Printf("Processing new and deleted decisions . . .")
		for {
			select {
			case <-t.Dying():
				log.Infoln("terminating bouncer process")
				return nil
			case decisions := <-bouncer.Stream:
				log.Infof("deleting '%d' decisions", len(decisions.Deleted))
				for _, decision := range decisions.Deleted {
					if err := custom.Delete(decision); err != nil {
						log.Errorf("unable to delete decision for '%s': %s", *decision.Value, err)
					} else {
						log.Debugf("deleted '%s'", *decision.Value)
					}

				}
				log.Infof("adding '%d' decisions", len(decisions.New))
				for _, decision := range decisions.New {
					if err := custom.Add(decision); err != nil {
						log.Errorf("unable to insert decision for '%s': %s", *decision.Value, err)
					} else {
						log.Debugf("Adding '%s' for '%s'", *decision.Value, *decision.Duration)
					}
				}
				custom.ResetCache()
			}
		}
	})

	if config.Daemon == true {
		sent, err := daemon.SdNotify(false, "READY=1")
		if !sent && err != nil {
			log.Errorf("Failed to notify: %v", err)
		}
		HandleSignals(custom)
	}

	err = t.Wait()
	if err != nil {
		log.Fatalf("process return with error: %s", err)
	}
}
