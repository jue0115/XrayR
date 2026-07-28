package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/XrayR-project/XrayR/panel"
)

var (
	cfgFile string
	rootCmd = &cobra.Command{
		Use: "XrayR",
		Run: func(cmd *cobra.Command, args []string) {
			if err := run(); err != nil {
				log.Fatal(err)
			}
		},
	}
)

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "Config file for XrayR.")
}

func getConfig() *viper.Viper {
	config := viper.New()

	// Set custom path and name
	if cfgFile != "" {
		configName := path.Base(cfgFile)
		configFileExt := path.Ext(cfgFile)
		configNameOnly := strings.TrimSuffix(configName, configFileExt)
		configPath := path.Dir(cfgFile)
		config.SetConfigName(configNameOnly)
		config.SetConfigType(strings.TrimPrefix(configFileExt, "."))
		config.AddConfigPath(configPath)
		// Set ASSET Path and Config Path for XrayR
		os.Setenv("XRAY_LOCATION_ASSET", configPath)
		os.Setenv("XRAY_LOCATION_CONFIG", configPath)
	} else {
		// Set default config path
		config.SetConfigName("config")
		config.SetConfigType("yml")
		config.AddConfigPath(".")

	}

	if err := config.ReadInConfig(); err != nil {
		log.Panicf("Config file error: %s \n", err)
	}

	config.WatchConfig() // Watch the config

	return config
}

func run() error {
	showVersion()

	config := getConfig()
	panelConfig := &panel.Config{}
	if err := config.Unmarshal(panelConfig); err != nil {
		return fmt.Errorf("Parse config file %v failed: %s \n", cfgFile, err)
	}
	p := panel.New(panelConfig)
	lastTime := time.Now()
	var reloadAccess sync.Mutex
	config.OnConfigChange(func(e fsnotify.Event) {
		reloadAccess.Lock()
		defer reloadAccess.Unlock()
		// Discarding event received within a short period of time after receiving an event.
		if time.Now().After(lastTime.Add(3 * time.Second)) {
			// Hot reload function
			fmt.Println("Config file changed:", e.Name)
			lastTime = time.Now()
			nextConfig := &panel.Config{}
			if err := config.Unmarshal(nextConfig); err != nil {
				log.Printf("Ignore invalid hot reload configuration %v: %s", cfgFile, err)
				return
			}
			next := panel.New(nextConfig)
			old := p
			if err := old.Close(); err != nil {
				log.Printf("Old panel close reported errors: %s", err)
			}
			if err := next.Start(); err != nil {
				log.Printf("Hot reload failed, restoring previous configuration: %s", err)
				if rollbackErr := old.Start(); rollbackErr != nil {
					log.Printf("Previous configuration rollback failed: %s", rollbackErr)
				}
				return
			}
			p = next
			// The old core and its pools are now unreachable. Return free heap
			// pages to the OS once instead of forcing periodic collections.
			debug.FreeOSMemory()
		}
	})
	if err := p.Start(); err != nil {
		return err
	}
	defer func() {
		if err := p.Close(); err != nil {
			log.Printf("Panel close failed: %s", err)
		}
	}()

	// Release temporary allocations retained while loading the initial config.
	debug.FreeOSMemory()
	// Running backend
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, os.Kill, syscall.SIGTERM)
	<-osSignals

	return nil
}

func Execute() error {
	return rootCmd.Execute()
}
