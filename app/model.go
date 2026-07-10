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

func GetCluster(callback *Callback, cluster *Cluster) (*Cluster, error) {
	// obtain read lock on whole cluster tree
	cluster.lock.RLock()
	callback.Add(cluster.lock.RUnlock)

	if !cluster.OK {
		return nil, fmt.Errorf("cluster state is invalid")
	}

	for _, node := range cluster.Nodes {
		node.lock.RLock()
		defer node.lock.RUnlock()
		callback.Add(node.lock.RUnlock)
		for _, instance := range node.Instances {
			instance.lock.RLock()
			callback.Add(instance.lock.RUnlock)
		}
	}

	return cluster, nil
}

func (cluster *Cluster) Sync() error {
	// obtain write lock on whole tree
	cluster.lock.Lock()
	defer cluster.lock.Unlock()
	for _, n := range cluster.Nodes {
		n.lock.Lock()
		defer n.lock.Unlock()
		for _, i := range n.Instances {
			i.lock.Lock()
			defer i.lock.Unlock()
		}
	}

	err := cluster.Build()
	if err != nil {
		cluster.OK = false
		return err
	}

	err = cluster.ResolvePoolMembership()
	if err != nil {
		cluster.OK = false
		return err
	}

	cluster.OK = true

	return nil
}

func (cluster *Cluster) Build() error {
	start := time.Now()

	cluster.Nodes = make(map[string]*Node)

	wg, _ := errgroup.WithContext(context.Background())

	// get all nodes
	nodes, err := cluster.pve.Nodes()
	if err != nil {
		return err
	}

	// for each node:
	for _, nodeName := range nodes {
		wg.Go(func() error {
			// rebuild node
			node := Node{}

			node.Name = nodeName
			node.cluster = cluster

			err := node.Build()
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] error encountered while syncing node %s: %s", nodeName, err)
			}

			cluster.NodesLock.Lock()
			cluster.Nodes[nodeName] = &node
			cluster.NodesLock.Unlock()

			return err
		})
	}

	err = wg.Wait()
	if err != nil {
		cluster.lock.Unlock()
		return err
	}

	log.Printf("[INFO] built cluster in %d ms", time.Since(start).Milliseconds())

	return nil
}

func (cluster *Cluster) ResolvePoolMembership() error {
	start := time.Now()

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

	log.Printf("[INFO] resovled cluster instance pool memberships in %d ms", time.Since(start).Milliseconds())

	return nil
}

func GetNode(callback *Callback, cluster *Cluster, nodeName string) (*Node, error) {
	cluster.lock.RLock()
	callback.Add(cluster.lock.RUnlock)

	if !cluster.OK {
		return nil, fmt.Errorf("cluster state is invalid")
	}

	// get node
	node, ok := cluster.Nodes[nodeName]
	if !ok {
		return nil, fmt.Errorf("%s not in cluster", nodeName)
	}

	node.lock.RLock()
	callback.Add(node.lock.RUnlock)
	for _, instance := range node.Instances {
		instance.lock.RLock()
		callback.Add(instance.lock.RUnlock)
	}

	return node, nil
}

func (node *Node) Sync() error {
	cluster := node.cluster

	// obtain write lock on this node subtree, and obtain write lock all instances for ResolvePoolMembership, obtain read lock on everything else
	cluster.lock.Lock() // todo relax
	defer cluster.lock.Unlock()
	for _, n := range cluster.Nodes {
		if n != node { // if other node, read lock
			n.lock.RLock()
			defer n.lock.RUnlock()
		} else { // if this node, write lock
			n.lock.Lock()
			defer n.lock.Unlock()
		}
		for _, i := range n.Instances { // need write lock on all instances
			i.lock.Lock()
			defer i.lock.Unlock()
		}
	}

	err := node.Build()
	if err != nil {
		cluster.OK = false
		return err
	}

	err = cluster.ResolvePoolMembership()
	if err != nil {
		cluster.OK = false
		return err
	}

	cluster.OK = true
	return nil
}

func (node *Node) Build() error {
	start := time.Now()

	nodeName := node.Name
	cluster := node.cluster

	err := cluster.pve.Node(node, nodeName)
	if err != nil && cluster.Nodes[nodeName] == nil { // node is unreachable and did not exist previously
		// return an error because we requested to sync a node that was not already in the cluster
		return fmt.Errorf("error retrieving %s: %s", nodeName, err.Error())
	}

	wg, _ := errgroup.WithContext(context.Background())

	if err != nil && cluster.Nodes[nodeName] != nil { // node is unreachable and did exist previously
		// assume the node is down or gone and delete from cluster
		delete(cluster.Nodes, nodeName)
		return nil
	}

	// get node's VMs
	vms, err := node.VirtualMachines()
	if err != nil {
		return err

	}
	for _, vmid := range vms {
		wg.Go(func() error {

			instanceID := InstanceID(paas.SafeUint64(vmid))
			instance := Instance{}
			instance.lock.Lock()

			instance.VMID = instanceID
			instance.Type = VM
			instance.node = node

			err := instance.Build()
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] error encountered while syncing vm %s.%d: %s", nodeName, vmid, err)
			}

			node.Instances[instanceID] = &instance

			instance.lock.Unlock()

			return nil
		})
	}

	// get node's CTs
	cts, err := node.Containers()
	if err != nil {
		return err
	}
	for _, vmid := range cts {
		wg.Go(func() error {
			instanceID := InstanceID(paas.SafeUint64(vmid))
			instance := Instance{}
			instance.lock.Lock()

			instance.VMID = instanceID
			instance.Type = CT
			instance.node = node

			err := instance.Build()
			if err != nil { // if an error was encountered, continue and log the error
				log.Printf("[ERR ] error encountered while syncing ct %s.%d: %s", nodeName, vmid, err)
			}

			node.InstancesLock.Lock()
			node.Instances[instanceID] = &instance
			node.InstancesLock.Unlock()

			instance.lock.Unlock()

			return nil
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

	log.Printf("[INFO] built node %s in %d ms", node.Name, time.Since(start).Milliseconds())

	return nil
}

func GetInstance(callback *Callback, cluster *Cluster, nodeName string, vmid uint64) (*Instance, error) {
	// obtain read lock on whole tree, but do not hold cluster and node read lock indefinitely
	cluster.lock.RLock()
	callback.Add(cluster.lock.RUnlock)

	if !cluster.OK {
		return nil, fmt.Errorf("cluster state is invalid")
	}

	// get node
	node, ok := cluster.Nodes[nodeName]
	if !ok {
		return nil, fmt.Errorf("%s not in cluster", nodeName)
	}
	node.lock.RLock()
	callback.Add(node.lock.RUnlock)

	// get instance
	instance, ok := node.Instances[InstanceID(vmid)]
	if !ok {
		return nil, fmt.Errorf("vmid %d not in node %s", vmid, node.Name)
	} else {
		instance.lock.RLock()
		callback.Add(instance.lock.RUnlock)
		return instance, nil
	}
}

func (instance *Instance) Sync() error {
	cluster := instance.node.cluster

	// obtain write lock on this instance subtree, and obtain write lock all instances for ResolvePoolMembership, obtain read lock on everything else
	cluster.lock.Lock() // todo relax
	defer cluster.lock.Unlock()
	for _, n := range cluster.Nodes {
		n.lock.RLock()
		defer n.lock.RUnlock()
		for _, i := range n.Instances { // need write lock on all instances
			i.lock.Lock()
			defer i.lock.Unlock()
		}
	}

	err := instance.Build()
	if err != nil {
		cluster.OK = false
		return err
	}

	err = cluster.ResolvePoolMembership()
	if err != nil {
		cluster.OK = false
		return err
	}

	cluster.OK = true
	return nil
}

func (instance *Instance) Build() error {
	start := time.Now()

	vmid := instance.VMID
	instancetype := instance.Type
	node := instance.node
	var err error
	switch instancetype {
	case VM:
		err = node.VirtualMachine(instance, vmid)
	case CT:
		err = node.Container(instance, vmid)

	}

	if err != nil && node.Instances[vmid] == nil { // instance is unreachable and did not exist previously
		// return an error because we requested to sync an instance that was not already in the cluster
		return fmt.Errorf("error retrieving %s.%d: %s", node.Name, vmid, err.Error())
	}

	wg, _ := errgroup.WithContext(context.Background())

	if err != nil && node.Instances[vmid] != nil { // node is unreachable and did exist previously
		// assume the instance is gone and delete from cluster
		log.Printf("[ERR ] error retrieving %s.%d: %s", node.Name, vmid, err)
		delete(node.Instances, vmid)
		return nil
	}

	for volid := range instance.configDisks {
		wg.Go(func() error {
			err := instance.BuildVolume(node, volid)
			if err != nil {
				log.Printf("[ERR ] error rebuilding volume %s: %s", volid, err)
			}
			return err
		})
	}

	for netid := range instance.configNets {
		wg.Go(func() error {
			err := instance.BuildNet(node, netid)
			if err != nil {
				log.Printf("[ERR ] error rebuilding net %s: %s", netid, err)
				return err
			}
			return err
		})
	}

	for deviceid := range instance.configHostPCIs {
		wg.Go(func() error {
			err := instance.BuildDevice(node, deviceid)
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
		err := instance.BuildBoot(node)
		if err != nil {
			log.Printf("[ERR ] error rebuilding boot: %s", err)
			return err
		}
	}

	instance.node = node

	log.Printf("[INFO] built instance %s.%d in %d ms", instance.node.Name, instance.VMID, time.Since(start).Milliseconds())

	return nil
}

func (instance *Instance) BuildVolume(node *Node, volid string) error {
	volumeDataString := instance.configDisks[volid]

	volume, err := GetVolumeInfo(node, volumeDataString)
	if err != nil {
		return err
	}

	voltype := AnyPrefixes(volid, paas.VolumeTypes)
	volume.Type = voltype
	volume.Volume_ID = VolumeID(volid)

	instance.VolumesLock.Lock()
	instance.Volumes[VolumeID(volid)] = volume
	instance.VolumesLock.Unlock()

	return nil
}

func (instance *Instance) BuildNet(node *Node, netid string) error {
	net := instance.configNets[netid]

	netinfo, err := GetNetInfo(net)
	netinfo.Net_ID = NetID(netid)
	if err != nil {
		return nil
	}

	instance.NetsLock.Lock()
	instance.Nets[NetID(netid)] = netinfo
	instance.NetsLock.Unlock()

	return nil
}

func (instance *Instance) BuildDevice(node *Node, deviceid string) error {
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

	instance.DevicesLock.Lock()
	instance.Devices[DeviceID(instanceDeviceBusID)].Device_ID = DeviceID(deviceid)
	instance.DevicesLock.Unlock()

	return nil
}

func (instance *Instance) BuildBoot(node *Node) error {
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
