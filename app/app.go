package app

import (
	"encoding/gob"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luthermonson/go-proxmox"
)

const APIVersion string = "1.0.0"

var client ProxmoxClient

func Run() {
	gob.Register(proxmox.Client{})
	gin.SetMode(gin.ReleaseMode)

	configPath := flag.String("config", "config.json", "path to config.json file")
	flag.Parse()

	config := GetConfig(*configPath)
	log.Printf("[INFO] initialized config from %s", *configPath)

	token := fmt.Sprintf(`%s@%s!%s`, config.PVE.Token.User, config.PVE.Token.Realm, config.PVE.Token.ID)
	client = NewClient(config.PVE.URL, token, config.PVE.Token.Secret)

	router := gin.Default()

	cluster := Cluster{}
	cluster.Init(client)
	start := time.Now()
	log.Printf("[INFO] starting cluster sync\n")
	err := cluster.Sync()
	if err != nil {
		log.Printf("[ERR ] error encountered while syncing cluster: %s", err)
	} else {
		log.Printf("[INFO] synced cluster in %d ms\n", time.Since(start).Milliseconds())
	}

	// set repeating update for full rebuilds
	ticker := time.NewTicker(time.Duration(config.ReloadInterval) * time.Second)
	log.Printf("[INFO] initialized cluster sync interval of %ds", config.ReloadInterval)
	channel := make(chan bool)
	go func() {
		for {
			select {
			case <-channel:
				return
			case <-ticker.C:
				start := time.Now()
				log.Printf("[INFO] starting cluster sync\n")
				err := cluster.Sync()
				if err != nil {
					log.Printf("[ERR ] error encountered while syncing cluster: %s", err)
				} else {
					log.Printf("[INFO] synced cluster in %d ms", time.Since(start).Milliseconds())
				}

			}
		}
	}()

	router.GET("/version", func(c *gin.Context) {
		PVEVersion, err := client.Version()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusOK, gin.H{"api-version": APIVersion, "pve-version": PVEVersion})
		}
	})

	router.GET("/", func(c *gin.Context) {
		v, err := cluster.Get()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		} else {
			c.JSON(http.StatusOK, gin.H{"cluster": v})
			return
		}
	})

	router.GET("/nodes/:node", func(c *gin.Context) {
		nodeid := c.Param("node")
		node, err := cluster.GetNode(nodeid)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		} else {
			c.JSON(http.StatusOK, gin.H{"node": node})
			return
		}
	})

	router.GET("/nodes/:node/instances/:vmid", func(c *gin.Context) {
		nodeid := c.Param("node")
		vmid, err := strconv.ParseUint(c.Param("vmid"), 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s could not be converted to vmid (uint)", c.Param("instance"))})
			return
		}
		instance, err := cluster.GetInstance(nodeid, vmid)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		} else {
			c.JSON(http.StatusOK, gin.H{"instance": instance})
			return
		}
	})

	router.POST("/sync", func(c *gin.Context) {
		start := time.Now()
		log.Printf("[INFO] starting cluster sync\n")
		err := cluster.Sync()
		if err != nil {
			log.Printf("[ERR ] failed to sync cluster: %s", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		} else {
			log.Printf("[INFO] synced cluster in %fs\n", time.Since(start).Seconds())
			return
		}
	})

	router.POST("/nodes/:node/sync", func(c *gin.Context) {
		nodeName := c.Param("node")
		start := time.Now()
		log.Printf("[INFO] starting %s sync\n", nodeName)
		err := SyncNode(&cluster, nodeName)
		if err != nil {
			log.Printf("[ERR ] failed to sync %s: %s", nodeName, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		} else {
			log.Printf("[INFO] synced %s in %fs\n", nodeName, time.Since(start).Seconds())
		}
	})

	router.POST("/nodes/:node/instances/:vmid/sync", func(c *gin.Context) {
		nodeName := c.Param("node")
		vmid, err := strconv.ParseUint(c.Param("vmid"), 10, 64)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("%s could not be converted to vmid (uint)", c.Param("instance"))})
			return
		}
		start := time.Now()
		log.Printf("[INFO] starting %s.%d sync\n", nodeName, vmid)
		err = SyncInstance(&cluster, nodeName, vmid)
		if err != nil {
			log.Printf("[ERR ] failed to sync %s.%d: %s", nodeName, vmid, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		} else {
			log.Printf("[INFO] synced %s in %fs\n", nodeName, time.Since(start).Seconds())
		}
	})

	log.Printf("[INFO] starting API listening on 0.0.0.0:%d", config.ListenPort)
	err = router.Run("0.0.0.0:" + strconv.Itoa(config.ListenPort))
	if err != nil {
		log.Fatalf("[Err] Error starting router: %s", err.Error())
	}
}
