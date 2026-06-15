package app

import (
	paas "proxmoxaas-common-lib"
	"sync"

	"github.com/luthermonson/go-proxmox"
)

// uses and aliases common resource types from proxmoxaas-common-lib
// except for Cluster, Node, and Instance which require mutex locks and additional fields

// add mutex and pve client
// override Nodes map to new custom Node type
type Cluster struct {
	paas.Cluster
	lock  sync.Mutex
	pve   ProxmoxClient
	Nodes map[string]*Node `json:"nodes"`
	OK    bool
}

// add mutex and pve api Node object
// override Instances map to new custom Instance type
type Node struct {
	lock sync.Mutex
	paas.Node
	Instances map[InstanceID]*Instance `json:"instances"`
	pvenode   *proxmox.Node
	storage   map[string][]*proxmox.StorageContent
}

type InstanceID = paas.InstanceID
type InstanceType = paas.InstanceType

const VM InstanceType = paas.VM
const CT InstanceType = paas.CT

// add mutex and various config objects
type Instance struct {
	lock sync.Mutex
	paas.Instance
	pveconfig      any
	configDisks    map[string]string
	configNets     map[string]string
	configHostPCIs map[string]string
	configBoot     string
}

type VolumeID = paas.VolumeID
type Volume = paas.Volume
type NetID = paas.NetID
type Net = paas.Net
type DeviceID = paas.DeviceID
type DeviceBus = paas.DeviceBus
type Device = paas.Device
type FunctionID = paas.FunctionID
type Function = paas.Function
type BootOrder = paas.BootOrder
