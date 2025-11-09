package app

import (
	"fmt"
	"log"
	"strings"
)

func (cluster *Cluster) Init(pve ProxmoxClient) {
	cluster.pve = pve
}

func (cluster *Cluster) Get() (*Cluster, error) {
	cluster_ch := make(chan *Cluster)
	err_ch := make(chan error)

	go func() {
		// aquire cluster lock
		cluster.lock.Lock()
		defer cluster.lock.Unlock()
		cluster_ch <- cluster
		err_ch <- nil
	}()

	return <-cluster_ch, <-err_ch
}

// hard sync cluster
func (cluster *Cluster) Sync() error {
	err_ch := make(chan error)

	go func() {
		// aquire lock on cluster, release on return
		cluster.lock.Lock()
		defer cluster.lock.Unlock()

		cluster.Nodes = make(map[string]*Node)

		// get all nodes
		nodes, err := cluster.pve.Nodes()
		if err != nil {
			err_ch <- err
			return
		}
		// for each node:
		for _, hostName := range nodes {
			// rebuild node
			err := cluster.RebuildNode(hostName)
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] %s", err)
			} else { // otherwise log success
				log.Printf("[INFO] successfully synced node %s", hostName)
			}
		}
		err_ch <- nil
	}()

	return <-err_ch
}

// get a node in the cluster
func (cluster *Cluster) GetNode(hostName string) (*Node, error) {
	host_ch := make(chan *Node)
	err_ch := make(chan error)

	go func() {
		// aquire cluster lock
		cluster.lock.Lock()
		defer cluster.lock.Unlock()
		// get host
		host, ok := cluster.Nodes[hostName]
		if !ok {
			host_ch <- nil
			err_ch <- fmt.Errorf("%s not in cluster", hostName)
		} else {
			// aquire host lock to wait in case of a concurrent write
			host.lock.Lock()
			defer host.lock.Unlock()

			host_ch <- host
			err_ch <- nil
		}
	}()

	return <-host_ch, <-err_ch
}

// hard sync node
// returns error if the node could not be reached
func (cluster *Cluster) RebuildNode(hostName string) error {
	err_ch := make(chan error)

	go func() {
		host, err := cluster.pve.Node(hostName)
		if err != nil && cluster.Nodes[hostName] == nil { // host is unreachable and did not exist previously
			// return an error because we requested to sync a node that was not already in the cluster
			err_ch <- fmt.Errorf("error retrieving %s: %s", hostName, err.Error())
		}

		// aquire lock on host, release on return
		host.lock.Lock()
		defer host.lock.Unlock()

		if err != nil && cluster.Nodes[hostName] != nil { // host is unreachable and did exist previously
			// assume the node is down or gone and delete from cluster
			delete(cluster.Nodes, hostName)
			err_ch <- nil
		}

		cluster.Nodes[hostName] = host

		// get node's VMs
		vms, err := host.VirtualMachines()
		if err != nil {
			err_ch <- err

		}
		for _, vmid := range vms {
			err := host.RebuildInstance(VM, vmid)
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] %s", err)
			} else {
				log.Printf("[INFO] successfully synced vm %s.%d", hostName, vmid)
			}
		}

		// get node's CTs
		cts, err := host.Containers()
		if err != nil {
			err_ch <- err
		}
		for _, vmid := range cts {
			err := host.RebuildInstance(CT, vmid)
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] %s", err)
			} else {
				log.Printf("[INFO] successfully synced ct %s.%d", hostName, vmid)
			}
		}

		// check node device reserved by iterating over each function, we will assume that a single reserved function means the device is also reserved
		for _, device := range host.Devices {
			reserved := false
			for _, function := range device.Functions {
				reserved = reserved || function.Reserved
			}
			device.Reserved = reserved
		}

		err_ch <- nil
	}()
	return <-err_ch
}

func (host *Node) GetInstance(vmid uint) (*Instance, error) {
	instance_ch := make(chan *Instance)
	err_ch := make(chan error)

	go func() {
		// aquire host lock
		host.lock.Lock()
		defer host.lock.Unlock()
		// get instance
		instance, ok := host.Instances[InstanceID(vmid)]
		if !ok {
			instance_ch <- nil
			err_ch <- fmt.Errorf("vmid %d not in host %s", vmid, host.Name)
		} else {
			// aquire instance lock to wait in case of a concurrent write
			instance.lock.Lock()
			defer instance.lock.Unlock()

			instance_ch <- instance
			err_ch <- nil
		}
	}()

	return <-instance_ch, <-err_ch
}

// hard sync instance
// returns error if the instance could not be reached
func (host *Node) RebuildInstance(instancetype InstanceType, vmid uint) error {
	err_ch := make(chan error)

	go func() {
		instanceID := InstanceID(vmid)
		var instance *Instance
		var err error
		if instancetype == VM {
			instance, err = host.VirtualMachine(vmid)
		} else if instancetype == CT {
			instance, err = host.Container(vmid)

		}

		if err != nil && host.Instances[instanceID] == nil { // instance is unreachable and did not exist previously
			// return an error because we requested to sync an instance that was not already in the cluster
			err_ch <- fmt.Errorf("error retrieving %s.%d: %s", host.Name, instanceID, err.Error())
		}

		// aquire lock on instance, release on return
		instance.lock.Lock()
		defer instance.lock.Unlock()

		if err != nil && host.Instances[instanceID] != nil { // host is unreachable and did exist previously
			// assume the instance is gone and delete from cluster
			delete(host.Instances, instanceID)
			err_ch <- nil
		}

		host.Instances[instanceID] = instance

		for volid := range instance.configDisks {
			instance.RebuildVolume(host, volid)
		}

		for netid := range instance.configNets {
			instance.RebuildNet(netid)
		}

		for deviceid := range instance.configHostPCIs {
			instance.RebuildDevice(host, deviceid)
		}

		if instance.Type == VM {
			instance.RebuildBoot()
		}

		err_ch <- nil
	}()

	return <-err_ch
}

func (instance *Instance) RebuildVolume(host *Node, volid string) error {
	volumeDataString := instance.configDisks[volid]

	volume, err := GetVolumeInfo(host, volumeDataString)
	if err != nil {
		return err
	}

	voltype := AnyPrefixes(volid, VolumeTypes)
	volume.Type = voltype
	volume.Volume_ID = VolumeID(volid)
	instance.Volumes[VolumeID(volid)] = volume

	return nil
}

func (instance *Instance) RebuildNet(netid string) error {
	net := instance.configNets[netid]

	netinfo, err := GetNetInfo(net)
	netinfo.Net_ID = NetID(netid)
	if err != nil {
		return nil
	}

	instance.Nets[NetID(netid)] = netinfo

	return nil
}

func (instance *Instance) RebuildDevice(host *Node, deviceid string) {
	instanceDevice, ok := instance.configHostPCIs[deviceid]
	if !ok { // if device does not exist
		log.Printf("[WARN] %s not found in devices", deviceid)
		return
	}

	hostDeviceBusID := DeviceID(strings.Split(instanceDevice, ",")[0])
	instanceDeviceBusID := DeviceID(deviceid)

	if DeviceBusIDIsSuperDevice(hostDeviceBusID) {
		instance.Devices[DeviceID(instanceDeviceBusID)] = host.Devices[DeviceBus(hostDeviceBusID)]
		for _, function := range instance.Devices[DeviceID(instanceDeviceBusID)].Functions {
			function.Reserved = true
		}
	} else {
		// sub function assignment not supported yet
	}

	instance.Devices[DeviceID(instanceDeviceBusID)].Device_ID = DeviceID(deviceid)
}

func (instance *Instance) RebuildBoot() {
	instance.Boot = BootOrder{}

	eligibleBoot := map[string]bool{}
	for k := range instance.Volumes {
		eligiblePrefix := AnyPrefixes(string(k), []string{"sata", "scsi", "ide"})
		if eligiblePrefix != "" {
			eligibleBoot[string(k)] = true
		}
	}
	for k := range instance.Nets {
		eligibleBoot[string(k)] = true
	}

	bootOrder := PVEObjectStringToMap(instance.configBoot)["order"]

	if len(bootOrder) != 0 {
		for bootTarget := range strings.SplitSeq(bootOrder, ";") { // iterate over elements selected for boot, add them to Enabled, and remove them from eligible boot target
			_, isEligible := eligibleBoot[bootTarget]
			if val, ok := instance.Volumes[VolumeID(bootTarget)]; ok && isEligible { // if the item is eligible and is in volumes
				instance.Boot.Enabled = append(instance.Boot.Enabled, val)
				delete(eligibleBoot, bootTarget)
			} else if val, ok := instance.Nets[NetID(bootTarget)]; ok && isEligible { // if the item is eligible and is in nets
				instance.Boot.Enabled = append(instance.Boot.Enabled, val)
				delete(eligibleBoot, bootTarget)
			} else { // item is not eligible for boot but is included in the boot order
				log.Printf("[WARN] encountered enabled but non-eligible boot target %s in instance %s\n", bootTarget, instance.Name)
				delete(eligibleBoot, bootTarget)
			}
		}
	}

	for bootTarget, isEligible := range eligibleBoot { // iterate over remaining items, add them to Disabled
		if val, ok := instance.Volumes[VolumeID(bootTarget)]; ok && isEligible { // if the item is eligible and is in volumes
			instance.Boot.Disabled = append(instance.Boot.Disabled, val)
		} else if val, ok := instance.Nets[NetID(bootTarget)]; ok && isEligible { // if the item is eligible and is in nets
			instance.Boot.Disabled = append(instance.Boot.Disabled, val)
		} else { // item is not eligible and is not already in the boot order, skip adding to model
			log.Printf("[WARN] encountered disabled and non-eligible boot target %s in instance %s\n", bootTarget, instance.Name)
		}
	}
}
