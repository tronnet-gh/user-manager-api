package app

import (
	paas "proxmoxaas-fabric/paas-common-lib"
	"sync"

	"github.com/luthermonson/go-proxmox"
)

type Cluster struct {
	paas.Cluster
	lock  sync.Mutex
	pve   ProxmoxClient
	Nodes map[string]*Node `json:"nodes"`
}

type Node struct {
	lock sync.Mutex
	paas.Node
	Instances map[InstanceID]*Instance `json:"instances"`
	pvenode   *proxmox.Node
}

type InstanceID = paas.InstanceID
type InstanceType = paas.InstanceType

const VM InstanceType = paas.VM
const CT InstanceType = paas.CT

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
