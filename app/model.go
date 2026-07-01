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
	if cluster.OK {
		return cluster, nil
	} else {
		return nil, fmt.Errorf("cluster state is invalid")
	}
}

func SyncCluster(cluster *Cluster) error {
	cluster.OK = false

	err := cluster.BuildCluster()
	if err != nil {
		return err
	}

	err = cluster.ResolvePoolMembership()
	if err != nil {
		return err
	}

	cluster.OK = true
	return nil
}

// hard sync cluster
func (cluster *Cluster) BuildCluster() error {
	// aquire lock on cluster, release on return
	cluster.lock.Lock()
	defer cluster.lock.Unlock()

	cluster.Nodes = make(map[string]*Node)

	wg, _ := errgroup.WithContext(context.Background())

	// get all nodes
	nodes, err := cluster.pve.Nodes()
	if err != nil {
		cluster.lock.Unlock()
		return err
	}

	// for each node:
	for _, nodeName := range nodes {
		wg.Go(func() error {
			start := time.Now()
			// rebuild node
			err := cluster.BuildNode(nodeName)
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] error encountered while syncing node %s: %s", nodeName, err)
			} else {
				log.Printf("[INFO] synced node %s in %d ms", nodeName, time.Since(start).Milliseconds())
			}
			return err
		})
	}

	err = wg.Wait()
	if err != nil {
		cluster.lock.Unlock()
		return err
	}

	return nil
}

func (cluster *Cluster) ResolvePoolMembership() error {
	// aquire lock on cluster, release on return
	cluster.lock.Lock()
	defer cluster.lock.Unlock()

	//clear existing pool memberships
	for _, node := range cluster.Nodes {
		for _, instance := range node.Instances {
			instance.Pool = ""
		}
	}

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
				if instance.Pool != "" { // enforces that an instance cannot be in two different pools
					return fmt.Errorf("Instance %d is in pools %s and %s which is not supported", member.VMID, instance.Pool, pool.PoolID)
				}
				instance.Pool = pool.PoolID
				log.Printf("[INFO] resolved pool membership for vmid=%d pool=%s", member.VMID, pool.PoolID)
			}
		}
	}

	return nil
}

// get a node in the cluster
func (cluster *Cluster) GetNode(nodeName string) (*Node, error) {
	// aquire cluster lock
	cluster.lock.Lock()
	defer cluster.lock.Unlock()

	// get node
	node, ok := cluster.Nodes[nodeName]
	if !ok {
		return nil, fmt.Errorf("%s not in cluster", nodeName)
	} else {
		// aquire node lock to wait in case of a concurrent write
		node.lock.Lock()
		defer node.lock.Unlock()

		return node, nil
	}
}

func SyncNode(cluster *Cluster, nodeName string) error {
	cluster.OK = false

	err := cluster.BuildNode(nodeName)
	if err != nil {
		return err
	}

	err = cluster.ResolvePoolMembership()
	if err != nil {
		return err
	}

	cluster.OK = true
	return nil
}

// hard sync node
// returns error if the node could not be reached
func (cluster *Cluster) BuildNode(nodeName string) error {
	node, err := cluster.pve.Node(nodeName)
	if err != nil && cluster.Nodes[nodeName] == nil { // node is unreachable and did not exist previously
		// return an error because we requested to sync a node that was not already in the cluster
		return fmt.Errorf("error retrieving %s: %s", nodeName, err.Error())
	}

	// aquire lock on node, release on return
	node.lock.Lock()
	defer node.lock.Unlock()

	wg, _ := errgroup.WithContext(context.Background())

	if err != nil && cluster.Nodes[nodeName] != nil { // node is unreachable and did exist previously
		// assume the node is down or gone and delete from cluster
		delete(cluster.Nodes, nodeName)
		return nil
	}

	cluster.Nodes[nodeName] = node

	// get node's VMs
	vms, err := node.VirtualMachines()
	if err != nil {
		return err

	}
	for _, vmid := range vms {
		wg.Go(func() error {
			start := time.Now()
			err := node.BuildInstance(VM, vmid)
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] error encountered while syncing vm %s.%d: %s", nodeName, vmid, err)
			} else {
				log.Printf("[INFO] synced vm %s.%d in %d ms", nodeName, vmid, time.Since(start).Milliseconds())
			}
			return err
		})
	}

	// get node's CTs
	cts, err := node.Containers()
	if err != nil {
		return err
	}
	for _, vmid := range cts {
		wg.Go(func() error {
			start := time.Now()
			err := node.BuildInstance(CT, vmid)
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] error encountered while syncing ct %s.%d: %s", nodeName, vmid, err)
			} else {
				log.Printf("[INFO] synced ct %s.%d in %d ms", nodeName, vmid, time.Since(start).Milliseconds())

			}
			return err
		})
	}

	err = wg.Wait()
	if err != nil {
		return err
	}

	// check node device reserved by iterating over each function, we will assume that a single reserved function means the device is also reserved
	for _, device := range node.Devices {
		reserved := false
		for _, function := range device.Functions {
			reserved = reserved || function.Reserved
		}
		device.Reserved = reserved
	}

	node.cluster = cluster

	return nil
}

func (node *Node) GetInstance(vmid uint) (*Instance, error) {
	// aquire node lock
	node.lock.Lock()
	defer node.lock.Unlock()

	// get instance
	instance, ok := node.Instances[InstanceID(vmid)]
	if !ok {
		return nil, fmt.Errorf("vmid %d not in node %s", vmid, node.Name)
	} else {
		// aquire instance lock to wait in case of a concurrent write
		instance.lock.Lock()
		defer instance.lock.Unlock()

		return instance, nil
	}
}

func SyncInstance(cluster *Cluster, nodeName string, vmid uint) error {
	cluster.OK = false

	node, err := cluster.GetNode(nodeName)
	if err != nil {
		return err
	}

	instance, err := node.GetInstance(uint(vmid))
	if err != nil {
		return err
	}

	err = node.BuildInstance(instance.Type, uint(vmid))
	if err != nil {
		return err
	}

	err = cluster.ResolvePoolMembership()
	if err != nil {
		return err
	}

	cluster.OK = true
	return nil
}

// hard sync instance
// returns error if the instance could not be reached
func (node *Node) BuildInstance(instancetype InstanceType, vmid uint) error {
	instanceID := InstanceID(vmid)
	var instance *Instance
	var err error
	switch instancetype {
	case VM:
		instance, err = node.VirtualMachine(vmid)
	case CT:
		instance, err = node.Container(vmid)

	}

	if err != nil && node.Instances[instanceID] == nil { // instance is unreachable and did not exist previously
		// return an error because we requested to sync an instance that was not already in the cluster
		return fmt.Errorf("error retrieving %s.%d: %s", node.Name, instanceID, err.Error())
	}

	// aquire lock on instance, release on return
	instance.lock.Lock()
	defer instance.lock.Unlock()

	wg, _ := errgroup.WithContext(context.Background())

	if err != nil && node.Instances[instanceID] != nil { // node is unreachable and did exist previously
		// assume the instance is gone and delete from cluster
		delete(node.Instances, instanceID)
		return nil
	}

	node.Instances[instanceID] = instance

	for volid := range instance.configDisks {
		wg.Go(func() error {
			err = instance.RebuildVolume(node, volid)
			if err != nil {
				log.Printf("[ERR ] error rebuilding volume %s: %s", volid, err)
			}
			return err
		})
	}

	for netid := range instance.configNets {
		wg.Go(func() error {
			err = instance.RebuildNet(node, netid)
			if err != nil {
				log.Printf("[ERR ] error rebuilding net %s: %s", netid, err)
				return err
			}
			return err
		})
	}

	for deviceid := range instance.configHostPCIs {
		wg.Go(func() error {
			err = instance.RebuildDevice(node, deviceid)
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
		err = instance.RebuildBoot(node)
		if err != nil {
			log.Printf("[ERR ] error rebuilding boot: %s", err)
		}
		return err
	}

	instance.node = node
	return nil
}

func (instance *Instance) RebuildVolume(node *Node, volid string) error {
	volumeDataString := instance.configDisks[volid]

	volume, err := GetVolumeInfo(node, volumeDataString)
	if err != nil {
		return err
	}

	voltype := AnyPrefixes(volid, paas.VolumeTypes)
	volume.Type = voltype
	volume.Volume_ID = VolumeID(volid)
	instance.Volumes[VolumeID(volid)] = volume

	return nil
}

func (instance *Instance) RebuildNet(node *Node, netid string) error {
	net := instance.configNets[netid]

	netinfo, err := GetNetInfo(net)
	netinfo.Net_ID = NetID(netid)
	if err != nil {
		return nil
	}

	instance.Nets[NetID(netid)] = netinfo

	return nil
}

func (instance *Instance) RebuildDevice(node *Node, deviceid string) error {
	instanceDevice, ok := instance.configHostPCIs[deviceid]
	if !ok { // if device does not exist
		log.Printf("[WARN] %s not found in devices on node %s", deviceid, node.Name)
		return nil
	}

	hostDeviceBusID := DeviceID(strings.Split(instanceDevice, ",")[0])
	instanceDeviceBusID := DeviceID(deviceid)

	if DeviceBusIDIsSuperDevice(hostDeviceBusID) {
		instance.Devices[DeviceID(instanceDeviceBusID)] = node.Devices[DeviceBus(hostDeviceBusID)]
		for _, function := range instance.Devices[DeviceID(instanceDeviceBusID)].Functions {
			function.Reserved = true
		}
	} else {
		// sub function assignment not supported yet
	}

	instance.Devices[DeviceID(instanceDeviceBusID)].Device_ID = DeviceID(deviceid)

	return nil
}

func (instance *Instance) RebuildBoot(node *Node) error {
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
