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

	token := fmt.Sprintf(`%s@%s!%s`, config.PVE.Token.USER, config.PVE.Token.REALM, config.PVE.Token.ID)
	client = NewClient(config.PVE.URL, token, config.PVE.Token.Secret)

	router := gin.Default()

	cluster := Cluster{}
	cluster.Init(client)
	start := time.Now()
	log.Printf("[INFO] starting cluster sync\n")
	cluster.Sync()
	log.Printf("[INFO] synced cluster in %fs\n", time.Since(start).Seconds())

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
				cluster.Sync()
				log.Printf("[INFO] synced cluster in %fs\n", time.Since(start).Seconds())
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

		node, err := cluster.GetNode(nodeid)

		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		} else {
			instance, err := node.GetInstance(uint(vmid))
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
				return
			} else {
				c.JSON(http.StatusOK, gin.H{"instance": instance})
				return
			}
		}
	})

	router.POST("/sync", func(c *gin.Context) {
		//go func() {
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
		//}()
	})

	router.POST("/nodes/:node/sync", func(c *gin.Context) {
		nodeid := c.Param("node")
		//go func() {
		start := time.Now()
		log.Printf("[INFO] starting %s sync\n", nodeid)
		err := cluster.RebuildNode(nodeid)
		if err != nil {
			log.Printf("[ERR ] failed to sync %s: %s", nodeid, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		} else {
			log.Printf("[INFO] synced %s in %fs\n", nodeid, time.Since(start).Seconds())
			return
		}
		//}()
	})

	router.POST("/nodes/:node/instances/:vmid/sync", func(c *gin.Context) {
		nodeid := c.Param("node")
		vmid, err := strconv.ParseUint(c.Param("vmid"), 10, 64)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("%s could not be converted to vmid (uint)", c.Param("instance"))})
			return
		}

		//go func() {
		start := time.Now()
		log.Printf("[INFO] starting %s.%d sync\n", nodeid, vmid)

		node, err := cluster.GetNode(nodeid)
		if err != nil {
			log.Printf("[ERR ] failed to sync %s.%d: %s", nodeid, vmid, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}

		instance, err := node.GetInstance(uint(vmid))
		if err != nil {
			log.Printf("[ERR ] failed to sync %s.%d: %s", nodeid, vmid, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}

		err = node.RebuildInstance(instance.Type, uint(vmid))
		if err != nil {
			log.Printf("[ERR ] failed to sync %s.%d: %s", nodeid, vmid, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		} else {
			log.Printf("[INFO] synced %s.%d in %fs\n", nodeid, vmid, time.Since(start).Seconds())
			return
		}
		//}()
	})

	log.Printf("[INFO] starting API listening on 0.0.0.0:%d", config.ListenPort)
	router.Run("0.0.0.0:" + strconv.Itoa(config.ListenPort))
}
