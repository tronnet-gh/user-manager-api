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
	log.Printf("[INFO] starting cluster sync\n")
	err := cluster.Sync()
	if err != nil {
		log.Printf("[ERR ] error encountered while syncing cluster: %s", err)
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
				log.Printf("[INFO] starting cluster sync\n")
				err := cluster.Sync()
				if err != nil {
					log.Printf("[ERR ] error encountered while syncing cluster: %s", err)
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
		callback := Callback{}
		v, err := GetCluster(&callback, &cluster)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusOK, gin.H{"cluster": v})
		}
		callback.Invoke()
	})

	router.GET("/nodes/:node", func(c *gin.Context) {
		nodeid := c.Param("node")
		callback := Callback{}
		node, err := GetNode(&callback, &cluster, nodeid)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusOK, gin.H{"node": node})
		}
		callback.Invoke()
	})

	router.GET("/nodes/:node/instances/:vmid", func(c *gin.Context) {
		nodeid := c.Param("node")
		vmid, err := strconv.ParseUint(c.Param("vmid"), 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s could not be converted to vmid (uint)", c.Param("instance"))})
			return
		}
		callback := Callback{}
		instance, err := GetInstance(&callback, &cluster, nodeid, vmid)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		} else {
			c.JSON(http.StatusOK, gin.H{"instance": instance})
		}
		callback.Invoke()
	})

	router.POST("/sync", func(c *gin.Context) {
		log.Printf("[INFO] starting cluster sync\n")
		err := cluster.Sync()
		if err != nil {
			log.Printf("[ERR ] failed to sync cluster: %s", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		} else {
			return
		}
	})

	router.POST("/nodes/:node/sync", func(c *gin.Context) {
		nodeName := c.Param("node")

		log.Printf("[INFO] starting %s sync\n", nodeName)

		callback := Callback{}
		node, err := GetNode(&callback, &cluster, nodeName)
		if err != nil {
			log.Printf("[ERR ] failed to sync %s: %s", nodeName, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			callback.Invoke()
			return
		}
		callback.Invoke()

		err = node.Sync()
		if err != nil {
			log.Printf("[ERR ] failed to sync %s: %s", nodeName, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		}
	})

	router.POST("/nodes/:node/instances/:vmid/sync", func(c *gin.Context) {
		nodeName := c.Param("node")
		vmid, err := strconv.ParseUint(c.Param("vmid"), 10, 64)
		if err != nil {
			log.Printf("[ERR ] failed to sync %s.%d: %s", nodeName, vmid, err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("%s could not be converted to vmid (uint)", c.Param("instance"))})
			return
		}

		log.Printf("[INFO] starting %s.%d sync\n", nodeName, vmid)

		callback := Callback{}
		instance, err := GetInstance(&callback, &cluster, nodeName, vmid)
		if err != nil {
			log.Printf("[ERR ] failed to sync %s.%d: %s", nodeName, vmid, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			callback.Invoke()
			return
		}
		callback.Invoke()

		err = instance.Sync()
		if err != nil {
			log.Printf("[ERR ] failed to sync %s.%d: %s", nodeName, vmid, err.Error())
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		}
	})

	log.Printf("[INFO] starting API listening on 0.0.0.0:%d", config.ListenPort)
	err = router.Run("0.0.0.0:" + strconv.Itoa(config.ListenPort))
	if err != nil {
		log.Fatalf("[Err] Error starting router: %s", err.Error())
	}
}
