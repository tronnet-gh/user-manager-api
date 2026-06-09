package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	paas "proxmoxaas-common-lib"

	"golang.org/x/sync/errgroup"
)

func (cluster *Cluster) Init(pve ProxmoxClient) {
	cluster.pve = pve
}

func (cluster *Cluster) Get() (*Cluster, error) {
	// aquire cluster lock
	cluster.lock.Lock()
	defer cluster.lock.Unlock()

	return cluster, nil
}

// hard sync cluster
func (cluster *Cluster) Sync() error {
	// aquire lock on cluster, release on return
	cluster.lock.Lock()

	cluster.Nodes = make(map[string]*Node)

	wg, _ := errgroup.WithContext(context.Background())

	// get all nodes
	nodes, err := cluster.pve.Nodes()
	if err != nil {
		cluster.lock.Unlock()
		return err
	}

	// for each node:
	for _, hostName := range nodes {
		wg.Go(func() error {
			start := time.Now()
			// rebuild node
			err := cluster.RebuildNode(hostName)
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] error encountered while syncing node %s: %s", hostName, err)
			} else {
				log.Printf("[INFO] synced node %s in %d ms", hostName, time.Since(start).Milliseconds())
			}
			return err
		})
	}

	err = wg.Wait()
	if err != nil {
		cluster.lock.Unlock()
		return err
	}

	cluster.lock.Unlock()

	err = cluster.ResolvePoolMembership()
	if err != nil {
		return err
	}

	return nil
}

func (cluster *Cluster) ResolvePoolMembership() error {
	// aquire lock on cluster, release on return
	cluster.lock.Lock()
	defer cluster.lock.Unlock()

	//resolve pool membership
	pools, err := cluster.pve.client.Pools(context.Background())
	if err != nil {
		return err
	}
	for _, pool := range pools {
		pool, err = cluster.pve.client.Pool(context.Background(), pool.PoolID)
		if err != nil {
			return err
		}
		for _, member := range pool.Members {
			if member.Type == "lxc" || member.Type == "qemu" {
				node, ok := cluster.Nodes[member.Node]
				if !ok {
					return fmt.Errorf("Instance %d has no node", member.VMID)
				}
				instance, ok := node.Instances[InstanceID(member.VMID)]
				if !ok {
					return fmt.Errorf("Instance %d claimed to be in node %s but was not", member.VMID, node.Name)
				}
				instance.Pool = pool.PoolID
				log.Printf("[INFO] resolved pool membership for vmid=%d pool=%s", member.VMID, pool.PoolID)
			}
		}
	}

	return nil
}

// get a node in the cluster
func (cluster *Cluster) GetNode(hostName string) (*Node, error) {
	// aquire cluster lock
	cluster.lock.Lock()
	defer cluster.lock.Unlock()
	// get host
	host, ok := cluster.Nodes[hostName]
	if !ok {
		return nil, fmt.Errorf("%s not in cluster", hostName)
	} else {
		// aquire host lock to wait in case of a concurrent write
		host.lock.Lock()
		defer host.lock.Unlock()

		return host, nil
	}
}

// hard sync node
// returns error if the node could not be reached
func (cluster *Cluster) RebuildNode(hostName string) error {
	host, err := cluster.pve.Node(hostName)
	if err != nil && cluster.Nodes[hostName] == nil { // host is unreachable and did not exist previously
		// return an error because we requested to sync a node that was not already in the cluster
		return fmt.Errorf("error retrieving %s: %s", hostName, err.Error())
	}

	// aquire lock on host, release on return
	host.lock.Lock()
	defer host.lock.Unlock()

	wg, _ := errgroup.WithContext(context.Background())

	if err != nil && cluster.Nodes[hostName] != nil { // host is unreachable and did exist previously
		// assume the node is down or gone and delete from cluster
		delete(cluster.Nodes, hostName)
		return nil
	}

	cluster.Nodes[hostName] = host

	// get node's VMs
	vms, err := host.VirtualMachines()
	if err != nil {
		return err

	}
	for _, vmid := range vms {
		wg.Go(func() error {
			start := time.Now()
			err := host.RebuildInstance(VM, vmid)
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] error encountered while syncing vm %s.%d: %s", hostName, vmid, err)
			} else {
				log.Printf("[INFO] synced vm %s.%d in %d ms", hostName, vmid, time.Since(start).Milliseconds())
			}
			return err
		})
	}

	// get node's CTs
	cts, err := host.Containers()
	if err != nil {
		return err
	}
	for _, vmid := range cts {
		wg.Go(func() error {
			start := time.Now()
			err := host.RebuildInstance(CT, vmid)
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] error encountered while syncing ct %s.%d: %s", hostName, vmid, err)
			} else {
				log.Printf("[INFO] synced ct %s.%d in %d ms", hostName, vmid, time.Since(start).Milliseconds())

			}
			return err
		})
	}

	err = wg.Wait()
	if err != nil {
		return err
	}

	// check node device reserved by iterating over each function, we will assume that a single reserved function means the device is also reserved
	for _, device := range host.Devices {
		reserved := false
		for _, function := range device.Functions {
			reserved = reserved || function.Reserved
		}
		device.Reserved = reserved
	}

	return nil
}

func (host *Node) GetInstance(vmid uint) (*Instance, error) {

	// aquire host lock
	host.lock.Lock()
	defer host.lock.Unlock()
	// get instance
	instance, ok := host.Instances[InstanceID(vmid)]
	if !ok {
		return nil, fmt.Errorf("vmid %d not in host %s", vmid, host.Name)
	} else {
		// aquire instance lock to wait in case of a concurrent write
		instance.lock.Lock()
		defer instance.lock.Unlock()

		return instance, nil
	}
}

// hard sync instance
// returns error if the instance could not be reached
func (host *Node) RebuildInstance(instancetype InstanceType, vmid uint) error {
	instanceID := InstanceID(vmid)
	var instance *Instance
	var err error
	switch instancetype {
	case VM:
		instance, err = host.VirtualMachine(vmid)
	case CT:
		instance, err = host.Container(vmid)

	}

	if err != nil && host.Instances[instanceID] == nil { // instance is unreachable and did not exist previously
		// return an error because we requested to sync an instance that was not already in the cluster
		return fmt.Errorf("error retrieving %s.%d: %s", host.Name, instanceID, err.Error())
	}

	// aquire lock on instance, release on return
	instance.lock.Lock()
	defer instance.lock.Unlock()

	wg, _ := errgroup.WithContext(context.Background())

	if err != nil && host.Instances[instanceID] != nil { // host is unreachable and did exist previously
		// assume the instance is gone and delete from cluster
		delete(host.Instances, instanceID)
		return nil
	}

	host.Instances[instanceID] = instance

	for volid := range instance.configDisks {
		wg.Go(func() error {
			err = instance.RebuildVolume(host, volid)
			if err != nil {
				log.Printf("[ERR ] error rebuilding volume %s: %s", volid, err)
			}
			return err
		})
	}

	for netid := range instance.configNets {
		wg.Go(func() error {
			err = instance.RebuildNet(host, netid)
			if err != nil {
				log.Printf("[ERR ] error rebuilding net %s: %s", netid, err)
				return err
			}
			return err
		})
	}

	for deviceid := range instance.configHostPCIs {
		wg.Go(func() error {
			err = instance.RebuildDevice(host, deviceid)
			if err != nil {
				log.Printf("[ERR ] error rebuilding pci %s: %s", deviceid, err)
			}
			return err
		})
	}

	err = wg.Wait()
	if err != nil {
		return err
	}

	if instance.Type == VM {
		err = instance.RebuildBoot(host)
		if err != nil {
			log.Printf("[ERR ] error rebuilding boot: %s", err)
		}
		return err
	} else {
		return nil
	}
}

func (instance *Instance) RebuildVolume(host *Node, volid string) error {
	volumeDataString := instance.configDisks[volid]

	volume, err := GetVolumeInfo(host, volumeDataString)
	if err != nil {
		return err
	}

	voltype := AnyPrefixes(volid, paas.VolumeTypes)
	volume.Type = voltype
	volume.Volume_ID = VolumeID(volid)
	instance.Volumes[VolumeID(volid)] = volume

	return nil
}

func (instance *Instance) RebuildNet(host *Node, netid string) error {
	net := instance.configNets[netid]

	netinfo, err := GetNetInfo(net)
	netinfo.Net_ID = NetID(netid)
	if err != nil {
		return nil
	}

	instance.Nets[NetID(netid)] = netinfo

	return nil
}

func (instance *Instance) RebuildDevice(host *Node, deviceid string) error {
	instanceDevice, ok := instance.configHostPCIs[deviceid]
	if !ok { // if device does not exist
		log.Printf("[WARN] %s not found in devices on node %s", deviceid, host.Name)
		return nil
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

	return nil
}

func (instance *Instance) RebuildBoot(host *Node) error {
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

	return nil
}
