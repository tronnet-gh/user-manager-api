package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/luthermonson/go-proxmox"
	"golang.org/x/sync/errgroup"
)

type ProxmoxClient struct {
	client *proxmox.Client
}

type PVEDevice struct { // used only for requests to PVE
	ID                    string `json:"id"`
	Device_Name           string `json:"device_name"`
	Vendor_Name           string `json:"vendor_name"`
	Subsystem_Device_Name string `json:"subsystem_device_name"`
	Subsystem_Vendor_Name string `json:"subsystem_vendor_name"`
}

type PVEProctype struct {
	Custom int
	Name   string
	Vendor string
}

func NewClient(url string, token string, secret string) ProxmoxClient {
	HTTPClient := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{},
		},
	}

	client := proxmox.NewClient(url,
		proxmox.WithHTTPClient(&HTTPClient),
		proxmox.WithAPIToken(token, secret),
	)

	return ProxmoxClient{client: client}
}

// Gets and returns the PVE API version
func (pve ProxmoxClient) Version() (proxmox.Version, error) {
	version, err := pve.client.Version(context.Background())
	if err != nil {
		return *version, err
	}
	return *version, err
}

// Gets all Nodes names
func (pve ProxmoxClient) Nodes() ([]string, error) {
	nodes, err := pve.client.Nodes(context.Background())
	if err != nil {
		return nil, err
	}

	names := []string{}
	for _, node := range nodes {
		names = append(names, node.Node)
	}

	return names, nil
}

// Gets a Node's resources but does not recursively expand instances
func (pve ProxmoxClient) Node(nodeName string) (*Node, error) {
	host := Node{}
	host.Devices = make(map[DeviceBus]*Device)
	host.Instances = make(map[InstanceID]*Instance)
	host.storage = make(map[string][]*proxmox.StorageContent)

	// get pve node
	node, err := pve.client.Node(context.Background(), nodeName)
	if err != nil {
		return &host, err
	}

	// get pve node devices
	devices := []PVEDevice{}
	err = pve.client.Get(context.Background(), fmt.Sprintf("/nodes/%s/hardware/pci", nodeName), &devices)
	if err != nil {
		return &host, err
	}
	// populate host devices from pve node devices
	for _, device := range devices {
		x := strings.Split(device.ID, ".")
		if len(x) != 2 { // this should always be true, but skip if not
			continue
		}
		deviceid := DeviceBus(x[0])
		functionid := FunctionID(x[1])
		if _, ok := host.Devices[deviceid]; !ok {
			host.Devices[deviceid] = &Device{
				Device_Bus:  deviceid,
				Device_Name: device.Device_Name,
				Vendor_Name: device.Vendor_Name,
				Functions:   make(map[FunctionID]*Function),
			}
		}
		host.Devices[deviceid].Functions[functionid] = &Function{
			Function_ID:   functionid,
			Function_Name: device.Subsystem_Device_Name,
			Vendor_Name:   device.Subsystem_Vendor_Name,
			Reserved:      false,
		}
	}

	// get pve node proctypes
	proctypes := []PVEProctype{}
	err = pve.client.Get(context.Background(), fmt.Sprintf("/nodes/%s/capabilities/qemu/cpu", nodeName), &proctypes)
	if err != nil {
		return &host, err
	}
	// populate host proctypes from pve node proctypes
	for _, proctype := range proctypes {
		host.Proctypes = append(host.Proctypes, proctype.Name)
	}

	// get all pve node storages
	wg, _ := errgroup.WithContext(context.Background())
	storages, err := node.Storages(context.Background())
	if err != nil {
		return &host, err
	}
	// populate host storage content from pve node storages
	for _, storage := range storages {
		wg.Go(func() error {
			if storage.Enabled == 1 && storage.Active == 1 {
				content, err := storage.GetContent(context.Background())
				if err != nil {
					return err
				}
				host.storage[storage.Name] = content
			}
			return nil
		})
	}
	err = wg.Wait()
	if err != nil {
		return &host, err
	}

	// set host basic values
	host.Name = node.Name
	host.Cores = SafeUint64(node.CPUInfo.CPUs)
	host.Memory = node.Memory.Total
	host.Swap = node.Swap.Total
	host.pvenode = node

	return &host, err
}

// Get all VM IDs on specified host
func (host *Node) VirtualMachines() ([]uint, error) {
	vms, err := host.pvenode.VirtualMachines(context.Background())
	if err != nil {
		return nil, err
	}
	ids := []uint{}
	for _, vm := range vms {
		ids = append(ids, uint(vm.VMID))
	}
	return ids, nil
}

// Get a VM's CPU, Memory but does not recursively link Devices, Disks, Drives, Nets
func (host *Node) VirtualMachine(VMID uint) (*Instance, error) {
	instance := Instance{}
	vm, err := host.pvenode.VirtualMachine(context.Background(), int(VMID))
	if err != nil {
		return &instance, err
	}

	config := vm.VirtualMachineConfig
	instance.configHostPCIs = config.HostPCIs
	instance.configNets = config.Nets
	instance.configDisks = MergeVMDisksAndUnused(config)
	instance.configBoot = config.Boot

	instance.pveconfig = config
	instance.Type = VM

	instance.Name = vm.Name
	instance.Proctype = vm.VirtualMachineConfig.CPU
	instance.Cores = SafeUint64(*vm.VirtualMachineConfig.Cores)
	instance.Memory = SafeUint64(int(vm.VirtualMachineConfig.Memory)) * MiB
	instance.Volumes = make(map[VolumeID]*Volume)
	instance.Nets = make(map[NetID]*Net)
	instance.Devices = make(map[DeviceID]*Device)

	return &instance, nil
}

func MergeVMDisksAndUnused(vmc *proxmox.VirtualMachineConfig) map[string]string {
	mergedDisks := vmc.MergeDisks()
	for k, v := range vmc.Unuseds {
		mergedDisks[k] = v
	}
	return mergedDisks
}

// Get all CT IDs on specified host
func (host *Node) Containers() ([]uint, error) {
	cts, err := host.pvenode.Containers(context.Background())
	if err != nil {
		return nil, err
	}
	ids := []uint{}
	for _, ct := range cts {
		ids = append(ids, uint(ct.VMID))
	}
	return ids, nil
}

// Get a CT's CPU, Memory, Swap but does not recursively link Devices, Disks, Drives, Nets
func (host *Node) Container(VMID uint) (*Instance, error) {
	instance := Instance{}
	ct, err := host.pvenode.Container(context.Background(), int(VMID))
	if err != nil {
		return &instance, err
	}

	config := ct.ContainerConfig
	instance.configHostPCIs = make(map[string]string)
	instance.configNets = config.Nets
	instance.configDisks = MergeCTDisksAndUnused(config)

	instance.pveconfig = config
	instance.Type = CT

	instance.Name = ct.Name
	instance.Cores = SafeUint64(ct.ContainerConfig.Cores)
	instance.Memory = SafeUint64(*ct.ContainerConfig.Memory) * MiB
	instance.Swap = SafeUint64(*ct.ContainerConfig.Swap) * MiB
	instance.Volumes = make(map[VolumeID]*Volume)
	instance.Nets = make(map[NetID]*Net)

	return &instance, nil
}

func MergeCTDisksAndUnused(cc *proxmox.ContainerConfig) map[string]string {
	mergedDisks := make(map[string]string)
	for k, v := range cc.Unuseds {
		mergedDisks[k] = v
	}
	for k, v := range cc.Mps {
		mergedDisks[k] = v
	}
	mergedDisks["rootfs"] = cc.RootFS
	return mergedDisks
}

// get volume format, size, volumeid, and storageid from instance volume data string (eg: local:100/vm-100-disk-0.raw ... )
func GetVolumeInfo(host *Node, volume string) (*Volume, error) {
	volumeData := Volume{}

	volumeObj := PVEObjectStringToMap(volume)
	volumeFile := volumeObj[""]
	volumeStorage := strings.Split(volumeFile, ":")[0]

	storage, ok := host.storage[volumeStorage]
	if !ok {
		return &volumeData, fmt.Errorf("volume %s claims to be in storage %s but storage was not found", volume, volumeStorage)
	}

	for _, c := range storage {
		if c.Volid == volumeFile {
			volumeData.Storage = volumeStorage
			volumeData.Format = c.Format
			volumeData.Size = c.Size
			volumeData.File = volumeFile
			volumeData.MP = volumeObj["mp"]
		}
	}

	return &volumeData, nil
}

func GetNetInfo(netstring string) (*Net, error) {
	n := Net{}

	netobj := PVEObjectStringToMap(netstring)

	rate, err := strconv.ParseUint(netobj["rate"], 10, 64)
	if err != nil {
		return &n, err
	}
	n.Rate = rate

	vlan, err := strconv.ParseUint(netobj["tag"], 10, 64)
	if err != nil {
		return &n, err
	}
	n.VLAN = vlan

	n.Value = netstring

	return &n, nil
}

// most pve objects (nets, disks, pcie, etc) have the following similar format:
// objname: v1,k2=v2,k3=v3,k4=v4 ...
// this function maps such strings to a map so that each individual key or value can be found more quickly
// in pcie or disks, the first value often does not have a key name, in such cases the key will be empty string ""
func PVEObjectStringToMap(objectstring string) map[string]string {
	objectmap := map[string]string{}
	for v := range strings.SplitSeq(objectstring, ",") {
		key := ""
		val := ""
		if strings.Contains(v, "=") {
			x := strings.Split(v, "=")
			key = x[0]
			val = x[1]
		} else {
			key = ""
			val = v
		}
		objectmap[key] = val
	}
	return objectmap
}
