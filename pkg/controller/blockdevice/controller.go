package blockdevice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	gocommon "github.com/harvester/go-common"
	ghwutil "github.com/jaypipes/ghw/pkg/util"
	longhornv1 "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	"github.com/sirupsen/logrus"
	"golang.org/x/exp/slices"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	diskv1 "github.com/harvester/node-disk-manager/pkg/apis/harvesterhci.io/v1beta1"
	"github.com/harvester/node-disk-manager/pkg/block"
	ctldiskv1 "github.com/harvester/node-disk-manager/pkg/generated/controllers/harvesterhci.io/v1beta1"
	ctllonghornv1 "github.com/harvester/node-disk-manager/pkg/generated/controllers/longhorn.io/v1beta2"
	"github.com/harvester/node-disk-manager/pkg/option"
	"github.com/harvester/node-disk-manager/pkg/utils"
)

const (
	blockDeviceHandlerName = "harvester-block-device-handler"
)

// semaphore is a simple semaphore implementation in channel
type semaphore struct {
	ch chan struct{}
}

// newSemaphore creates a new semaphore with the given capacity.
func newSemaphore(n uint) *semaphore {
	return &semaphore{
		ch: make(chan struct{}, n),
	}
}

// acquire a semaphore to prevent concurrent update
func (s *semaphore) acquire() bool {
	logrus.Debugf("Pre-acquire channel stats: %d/%d", len(s.ch), cap(s.ch))
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		// full
		return false
	}
}

// release the semaphore
func (s *semaphore) release() bool {
	select {
	case <-s.ch:
		return true
	default:
		// empty
		return false
	}
}

type DiskTags struct {
	diskTags    map[string][]string
	lock        *sync.RWMutex
	initialized bool
}

type Controller struct {
	Namespace string
	NodeName  string

	NodeCache ctllonghornv1.NodeCache
	Nodes     ctllonghornv1.NodeClient

	Blockdevices     ctldiskv1.BlockDeviceController
	BlockdeviceCache ctldiskv1.BlockDeviceCache
	BlockInfo        block.Info

	scanner   *Scanner
	semaphore *semaphore
}

type NeedMountUpdateOP int8

const (
	NeedMountUpdateNoOp NeedMountUpdateOP = 1 << iota
	NeedMountUpdateMount
	NeedMountUpdateUnmount

	errorCacheDiskTagsNotInitialized = "CacheDiskTags is not initialized"
)

func (f NeedMountUpdateOP) Has(flag NeedMountUpdateOP) bool {
	return f&flag != 0
}

var CacheDiskTags *DiskTags

func (d *DiskTags) DeleteDiskTags(dev string) {
	d.lock.Lock()
	defer d.lock.Unlock()

	delete(d.diskTags, dev)
}

func (d *DiskTags) UpdateDiskTags(dev string, tags []string) {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.diskTags[dev] = tags
}

func (d *DiskTags) UpdateInitialized() {
	d.lock.Lock()
	defer d.lock.Unlock()

	d.initialized = true
}

func (d *DiskTags) Initialized() bool {
	d.lock.RLock()
	defer d.lock.RUnlock()

	return d.initialized
}

func (d *DiskTags) GetDiskTags(dev string) []string {
	d.lock.RLock()
	defer d.lock.RUnlock()

	return d.diskTags[dev]
}

func (d *DiskTags) DevExist(dev string) bool {
	d.lock.RLock()
	defer d.lock.RUnlock()

	_, found := d.diskTags[dev]
	return found
}

// Register register the block device CRD controller
func Register(
	ctx context.Context,
	nodes ctllonghornv1.NodeController,
	bds ctldiskv1.BlockDeviceController,
	block block.Info,
	opt *option.Option,
	scanner *Scanner,
) error {
	CacheDiskTags = &DiskTags{
		diskTags:    make(map[string][]string),
		lock:        &sync.RWMutex{},
		initialized: false,
	}
	controller := &Controller{
		Namespace:        opt.Namespace,
		NodeName:         opt.NodeName,
		NodeCache:        nodes.Cache(),
		Nodes:            nodes,
		Blockdevices:     bds,
		BlockdeviceCache: bds.Cache(),
		BlockInfo:        block,
		scanner:          scanner,
		semaphore:        newSemaphore(opt.MaxConcurrentOps),
	}

	if err := scanner.Start(); err != nil {
		return err
	}

	utils.CallerWithCondLock(scanner.Cond, func() any {
		logrus.Infof("Wake up scanner first time to update CacheDiskTags ...")
		scanner.Cond.Signal()
		return nil
	})

	bds.OnChange(ctx, blockDeviceHandlerName, controller.OnBlockDeviceChange)
	bds.OnRemove(ctx, blockDeviceHandlerName, controller.OnBlockDeviceDelete)
	return nil
}

// OnBlockDeviceChange watch the block device CR on change and performing disk operations
// like mounting the disks to a desired path via ext4
func (c *Controller) OnBlockDeviceChange(_ string, device *diskv1.BlockDevice) (*diskv1.BlockDevice, error) {
	if device == nil || device.DeletionTimestamp != nil || device.Spec.NodeName != c.NodeName || device.Status.State == diskv1.BlockDeviceInactive {
		return nil, nil
	}

	// corrupted device could be skipped if we do not set ForceFormatted or Repaired
	if device.Status.DeviceStatus.FileSystem.Corrupted && !device.Spec.FileSystem.ForceFormatted && !device.Spec.FileSystem.Repaired {
		return nil, nil
	}

	if !CacheDiskTags.Initialized() {
		return nil, errors.New(errorCacheDiskTagsNotInitialized)
	}

	deviceCpy := device.DeepCopy()
	devPath, err := resolvePersistentDevPath(device)
	if err != nil {
		return nil, err
	}
	if devPath == "" {
		return nil, fmt.Errorf("failed to resolve persistent dev path for block device %s", device.Name)
	}
	filesystem := c.BlockInfo.GetFileSystemInfoByDevPath(devPath)
	devPathStatus := convertFSInfoToString(filesystem)
	logrus.Debugf("Get filesystem info from device %s, %s", devPath, devPathStatus)

	needFormat := deviceCpy.Spec.FileSystem.ForceFormatted && (deviceCpy.Status.DeviceStatus.FileSystem.Corrupted || deviceCpy.Status.DeviceStatus.FileSystem.LastFormattedAt == nil)
	if needFormat {
		logrus.Infof("Prepare to force format device %s", device.Name)
		err := c.forceFormat(deviceCpy, devPath, filesystem)
		if err != nil {
			err := fmt.Errorf("failed to force format device %s: %s", device.Name, err.Error())
			logrus.Error(err)
			diskv1.DeviceFormatting.SetError(deviceCpy, "", err)
			diskv1.DeviceFormatting.SetStatusBool(deviceCpy, false)
		}
		if !reflect.DeepEqual(device, deviceCpy) {
			logrus.Debugf("Update block device %s for new formatting state", device.Name)
			return c.Blockdevices.Update(deviceCpy)
		}
		return device, err
	}

	if needMountUpdate := needUpdateMountPoint(deviceCpy, filesystem); needMountUpdate != NeedMountUpdateNoOp {
		err := c.updateDeviceMount(deviceCpy, devPath, filesystem, needMountUpdate)
		if err != nil {
			err := fmt.Errorf("failed to update device mount %s: %s", device.Name, err.Error())
			logrus.Error(err)
			diskv1.DeviceMounted.SetError(deviceCpy, "", err)
			diskv1.DeviceMounted.SetStatusBool(deviceCpy, false)
		}
		if !reflect.DeepEqual(device, deviceCpy) {
			logrus.Debugf("Update block device %s for new formatting and mount state", device.Name)
			return c.Blockdevices.Update(deviceCpy)
		}
		return device, err
	}

	/*
	 * We use the needProvision to control first time provision.
	 * 1. `deviceCpy.Spec.FileSystem.Provisioned` is False.
	 * 2. updateDeviceStatus() would made `deviceCpy.Spec.FileSystem.Provisioned` be true and trigger Update
	 * 3. loop back and check `deviceCpy.Spec.FileSystem.Provisioned` again. (Now needProvision is true)
	 * 4. provision
	 *
	 * NOTE: we do not need to provision again for provisioned device so we should do another
	 *       check with `device.Status.ProvisionPhase`
	 */
	needProvision := deviceCpy.Spec.FileSystem.Provisioned
	switch {
	case needProvision && device.Status.ProvisionPhase == diskv1.ProvisionPhaseProvisioned:
		logrus.Infof("Prepare to check the new device tags %v with device: %s", deviceCpy.Spec.Tags, device.Name)
		DiskTagsSynced := gocommon.SliceContentCmp(deviceCpy.Spec.Tags, CacheDiskTags.GetDiskTags(device.Name))
		DiskTagsOnNodeMissed := func() bool {
			node, err := c.NodeCache.Get(c.Namespace, c.NodeName)
			if err != nil {
				// dont check, just provision
				return true
			}
			nodeDisk := node.Spec.Disks[device.Name]
			for _, tag := range deviceCpy.Spec.Tags {
				if !slices.Contains(nodeDisk.Tags, tag) {
					return true
				}
			}
			return false
		}
		if !DiskTagsSynced || (DiskTagsSynced && DiskTagsOnNodeMissed()) {
			logrus.Debugf("Prepare to update device %s because the Tags changed, Spec: %v, CacheDiskTags: %v", deviceCpy.Name, deviceCpy.Spec.Tags, CacheDiskTags.GetDiskTags(device.Name))
			if err := c.provisionDeviceToNode(deviceCpy); err != nil {
				err := fmt.Errorf("failed to update tags %v with device %s to node %s: %w", deviceCpy.Spec.Tags, device.Name, c.NodeName, err)
				logrus.Error(err)
				c.Blockdevices.EnqueueAfter(c.Namespace, device.Name, jitterEnqueueDelay())
			}
		}
	case needProvision && device.Status.ProvisionPhase == diskv1.ProvisionPhaseUnprovisioned:
		logrus.Infof("Prepare to provision device %s to node %s", device.Name, c.NodeName)
		if err := c.provisionDeviceToNode(deviceCpy); err != nil {
			err := fmt.Errorf("failed to provision device %s to node %s: %w", device.Name, c.NodeName, err)
			logrus.Error(err)
			diskv1.DiskAddedToNode.SetError(deviceCpy, "", err)
			diskv1.DiskAddedToNode.SetStatusBool(deviceCpy, false)
			c.Blockdevices.EnqueueAfter(c.Namespace, device.Name, jitterEnqueueDelay())
		}
	case !needProvision && device.Status.ProvisionPhase != diskv1.ProvisionPhaseUnprovisioned:
		logrus.Infof("Prepare to stop provisioning device %s to node %s", device.Name, c.NodeName)
		if err := c.unprovisionDeviceFromNode(deviceCpy); err != nil {
			err := fmt.Errorf("failed to stop provisioning device %s to node %s: %w", device.Name, c.NodeName, err)
			logrus.Error(err)
			diskv1.DiskAddedToNode.SetError(deviceCpy, "", err)
			diskv1.DiskAddedToNode.SetStatusBool(deviceCpy, false)
			c.Blockdevices.EnqueueAfter(c.Namespace, device.Name, jitterEnqueueDelay())
		}
	}

	if !reflect.DeepEqual(device, deviceCpy) {
		logrus.Debugf("Update block device %s for new provision state", device.Name)
		return c.Blockdevices.Update(deviceCpy)
	}

	// None of the above operations have resulted in an update to the device.
	// We therefore try to update the latest device status from the OS
	if err := c.updateDeviceStatus(deviceCpy, devPath); err != nil {
		return nil, err
	}

	if !reflect.DeepEqual(device, deviceCpy) {
		logrus.Debugf("Update block device %s for new device status", device.Name)
		return c.Blockdevices.Update(deviceCpy)
	}

	return nil, nil
}

func (c *Controller) updateDeviceMount(device *diskv1.BlockDevice, devPath string, filesystem *block.FileSystemInfo, needMountUpdate NeedMountUpdateOP) error {
	logrus.Infof("Prepare to try %s", convertMountStr(needMountUpdate))
	if device.Status.DeviceStatus.Partitioned {
		return fmt.Errorf("partitioned device is not supported, please use raw block device instead")
	}
	if needMountUpdate.Has(NeedMountUpdateUnmount) {
		logrus.Infof("Unmount device %s from path %s", device.Name, filesystem.MountPoint)
		if err := utils.UmountDisk(filesystem.MountPoint); err != nil {
			return err
		}
		diskv1.DeviceMounted.SetError(device, "", nil)
		diskv1.DeviceMounted.SetStatusBool(device, false)
	}
	if needMountUpdate.Has(NeedMountUpdateMount) {
		expectedMountPoint := extraDiskMountPoint(device)
		logrus.Infof("Mount deivce %s to %s", device.Name, expectedMountPoint)
		if err := utils.MountDisk(devPath, expectedMountPoint); err != nil {
			if utils.IsFSCorrupted(err) {
				logrus.Errorf("Target device may be corrupted, update FS info.")
				device.Status.DeviceStatus.FileSystem.Corrupted = true
				device.Spec.FileSystem.Repaired = false
			}
			return err
		}
		diskv1.DeviceMounted.SetError(device, "", nil)
		diskv1.DeviceMounted.SetStatusBool(device, true)
	}
	device.Status.DeviceStatus.FileSystem.Corrupted = false
	return c.updateDeviceFileSystem(device, devPath)
}

func (c *Controller) updateDeviceFileSystem(device *diskv1.BlockDevice, devPath string) error {
	if device.Status.DeviceStatus.FileSystem.Corrupted {
		// do not need to update other fields, we only need to update the corrupted flag
		return nil
	}
	filesystem := c.BlockInfo.GetFileSystemInfoByDevPath(devPath)
	if filesystem == nil {
		return fmt.Errorf("failed to get filesystem info from devPath %s", devPath)
	}
	if filesystem.MountPoint != "" && filesystem.Type != "" && !utils.IsSupportedFileSystem(filesystem.Type) {
		return fmt.Errorf("unsupported filesystem type %s", filesystem.Type)
	}

	device.Status.DeviceStatus.FileSystem.MountPoint = filesystem.MountPoint
	device.Status.DeviceStatus.FileSystem.Type = filesystem.Type
	device.Status.DeviceStatus.FileSystem.IsReadOnly = filesystem.IsReadOnly
	return nil
}

func valueExists(value string) bool {
	return value != "" && value != ghwutil.UNKNOWN
}

// forceFormat simply formats the device to ext4 filesystem
//
// - umount the block device if it is mounted
// - create ext4 filesystem on the block device
func (c *Controller) forceFormat(device *diskv1.BlockDevice, devPath string, filesystem *block.FileSystemInfo) error {
	if !c.semaphore.acquire() {
		logrus.Infof("Hit maximum concurrent count. Requeue device %s", device.Name)
		c.Blockdevices.EnqueueAfter(c.Namespace, device.Name, jitterEnqueueDelay())
		return nil
	}

	defer c.semaphore.release()

	// umount the disk if it is mounted
	if filesystem != nil && filesystem.MountPoint != "" {
		logrus.Infof("unmount %s for %s", filesystem.MountPoint, device.Name)
		if err := utils.UmountDisk(filesystem.MountPoint); err != nil {
			return err
		}
	}

	// make ext4 filesystem format of the partition disk
	logrus.Debugf("make ext4 filesystem format of device %s", device.Name)
	// Reuse UUID if possible to make the filesystem UUID more stable.
	//
	// The reason filesystem UUID needs to be stable is that if a disk
	// lacks WWN, NDM then needs a UUID to determine the unique identity
	// of the blockdevice CR.
	//
	// We don't reuse WWN as UUID here because we assume that WWN is
	// stable and permanent for a disk. Thefore, even if the underlying
	// device gets formatted and the filesystem UUID changes, it still
	// won't affect then unique identity of the blockdevice.
	var uuid string
	if !valueExists(device.Status.DeviceStatus.Details.WWN) {
		uuid = device.Status.DeviceStatus.Details.UUID
		if !valueExists(uuid) {
			uuid = device.Status.DeviceStatus.Details.PtUUID
		}
		if !valueExists(uuid) {
			// Reset the UUID to prevent "unknown" being passed down.
			uuid = ""
		}
	}
	if err := utils.MakeExt4DiskFormatting(devPath, uuid); err != nil {
		return err
	}

	// HACK: Update the UUID if it is reused.
	//
	// This makes the controller able to find then device after
	// a PtUUID is reused in `mkfs.ext4` as filesystem UUID.
	//
	// If the UUID is not updated within one-stop, the next
	// `OnBlockDeviceChange` is not able to find the device
	// because `status.DeviceStatus.Details.UUID` is missing.
	if uuid != "" {
		device.Status.DeviceStatus.Details.UUID = uuid
	}

	if err := c.updateDeviceFileSystem(device, devPath); err != nil {
		return err
	}
	diskv1.DeviceFormatting.SetError(device, "", nil)
	diskv1.DeviceFormatting.SetStatusBool(device, false)
	diskv1.DeviceFormatting.Message(device, "Done device ext4 filesystem formatting")
	device.Status.DeviceStatus.FileSystem.LastFormattedAt = &metav1.Time{Time: time.Now()}
	device.Status.DeviceStatus.Partitioned = false
	device.Status.DeviceStatus.FileSystem.Corrupted = false
	return nil
}

// provisionDeviceToNode adds a device to longhorn node as an additional disk.
func (c *Controller) provisionDeviceToNode(device *diskv1.BlockDevice) error {
	node, err := c.NodeCache.Get(c.Namespace, c.NodeName)
	if apierrors.IsNotFound(err) {
		node, err = c.Nodes.Get(c.Namespace, c.NodeName, metav1.GetOptions{})
	}
	if err != nil {
		return err
	}

	nodeCpy := node.DeepCopy()
	diskSpec := longhornv1.DiskSpec{
		Path:              extraDiskMountPoint(device),
		AllowScheduling:   true,
		EvictionRequested: false,
		StorageReserved:   0,
		Tags:              device.Spec.Tags,
	}

	updated := false
	if disk, found := node.Spec.Disks[device.Name]; found {
		respectedTags := []string{}
		if disk.Tags != nil {
			/* we should respect the disk Tags from LH */
			if CacheDiskTags.DevExist(device.Name) {
				for _, tag := range disk.Tags {
					if !slices.Contains(CacheDiskTags.GetDiskTags(device.Name), tag) {
						respectedTags = append(respectedTags, tag)
					}
				}
			} else {
				respectedTags = disk.Tags
			}
			logrus.Debugf("Previous disk tags only on LH: %+v, we should respect it.", respectedTags)
			diskSpec.Tags = gocommon.SliceDedupe(append(respectedTags, device.Spec.Tags...))
			updated = reflect.DeepEqual(disk, diskSpec)
		}
	}
	// **NOTE** we do the `DiskAddedToNode` check here if we failed to update the device.
	// That means the device status is not `Provisioned` but the LH node already has the disk.
	// That we would not do next update, to make the device `Provisioned`.
	if !updated || !diskv1.DiskAddedToNode.IsTrue(device) {
		// not updated means empty or different, we should update it.
		if !updated {
			nodeCpy.Spec.Disks[device.Name] = diskSpec
			if _, err = c.Nodes.Update(nodeCpy); err != nil {
				return err
			}
		}

		if !diskv1.DiskAddedToNode.IsTrue(device) {
			// Update if needed. If the info is alreay there, no need to update.
			msg := fmt.Sprintf("Added disk %s to longhorn node `%s` as an additional disk", device.Name, node.Name)
			device.Status.ProvisionPhase = diskv1.ProvisionPhaseProvisioned
			diskv1.DiskAddedToNode.SetError(device, "", nil)
			diskv1.DiskAddedToNode.SetStatusBool(device, true)
			diskv1.DiskAddedToNode.Message(device, msg)
		}
	}

	// update oldDiskTags
	CacheDiskTags.UpdateDiskTags(device.Name, device.Spec.Tags)

	return nil
}

// unprovisionDeviceFromNode removes a device from a longhorn node.
func (c *Controller) unprovisionDeviceFromNode(device *diskv1.BlockDevice) error {
	node, err := c.Nodes.Get(c.Namespace, c.NodeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Skip since the node is not there.
			return nil
		}
		return err
	}

	updateProvisionPhaseUnprovisioned := func() {
		msg := fmt.Sprintf("Disk not in longhorn node `%s`", c.NodeName)
		device.Status.ProvisionPhase = diskv1.ProvisionPhaseUnprovisioned
		diskv1.DiskAddedToNode.SetError(device, "", nil)
		diskv1.DiskAddedToNode.SetStatusBool(device, false)
		diskv1.DiskAddedToNode.Message(device, msg)
	}

	diskToRemove, ok := node.Spec.Disks[device.Name]
	if !ok {
		logrus.Infof("disk %s not in disks of longhorn node %s/%s", device.Name, c.Namespace, c.NodeName)
		updateProvisionPhaseUnprovisioned()
		return nil
	}

	isUnprovisioning := false
	for _, tag := range diskToRemove.Tags {
		if tag == utils.DiskRemoveTag {
			isUnprovisioning = true
			break
		}
	}

	if isUnprovisioning {
		if status, ok := node.Status.DiskStatus[device.Name]; ok && len(status.ScheduledReplica) == 0 {
			// Unprovision finished. Remove the disk.
			nodeCpy := node.DeepCopy()
			delete(nodeCpy.Spec.Disks, device.Name)
			if _, err := c.Nodes.Update(nodeCpy); err != nil {
				return err
			}
			updateProvisionPhaseUnprovisioned()
			logrus.Debugf("device %s is unprovisioned", device.Name)
		} else {
			// Still unprovisioning
			c.Blockdevices.EnqueueAfter(c.Namespace, device.Name, jitterEnqueueDelay())
			logrus.Debugf("device %s is unprovisioning, status: %+v, ScheduledReplica: %d", device.Name, node.Status.DiskStatus[device.Name], len(status.ScheduledReplica))
		}
	} else {
		// Start unprovisioing
		logrus.Debugf("Setup device %s to start unprovision", device.Name)
		diskToRemove.AllowScheduling = false
		diskToRemove.EvictionRequested = true
		diskToRemove.Tags = append(diskToRemove.Tags, utils.DiskRemoveTag)
		nodeCpy := node.DeepCopy()
		nodeCpy.Spec.Disks[device.Name] = diskToRemove
		if _, err := c.Nodes.Update(nodeCpy); err != nil {
			return err
		}
		msg := fmt.Sprintf("Stop provisioning device %s to longhorn node `%s`", device.Name, c.NodeName)
		device.Status.ProvisionPhase = diskv1.ProvisionPhaseUnprovisioning
		diskv1.DiskAddedToNode.SetError(device, "", nil)
		diskv1.DiskAddedToNode.SetStatusBool(device, false)
		diskv1.DiskAddedToNode.Message(device, msg)
	}

	return nil
}

func (c *Controller) updateDeviceStatus(device *diskv1.BlockDevice, devPath string) error {
	var newStatus diskv1.DeviceStatus
	var needAutoProvision bool

	switch device.Status.DeviceStatus.Details.DeviceType {
	case diskv1.DeviceTypeDisk:
		disk := c.BlockInfo.GetDiskByDevPath(devPath)
		bd := GetDiskBlockDevice(disk, c.NodeName, c.Namespace)
		newStatus = bd.Status.DeviceStatus
		autoProvisioned := c.scanner.ApplyAutoProvisionFiltersForDisk(disk)
		// Only disk can be auto-provisioned.
		needAutoProvision = c.scanner.NeedsAutoProvision(device, autoProvisioned)
	case diskv1.DeviceTypePart:
		parentDevPath, err := block.GetParentDevName(devPath)
		if err != nil {
			return fmt.Errorf("failed to get parent devPath for %s: %v", device.Name, err)
		}
		part := c.BlockInfo.GetPartitionByDevPath(parentDevPath, devPath)
		bd := GetPartitionBlockDevice(part, c.NodeName, c.Namespace)
		newStatus = bd.Status.DeviceStatus
	default:
		return fmt.Errorf("unknown device type %s", device.Status.DeviceStatus.Details.DeviceType)
	}

	oldStatus := device.Status.DeviceStatus
	lastFormatted := oldStatus.FileSystem.LastFormattedAt
	if lastFormatted != nil && newStatus.FileSystem.LastFormattedAt == nil {
		newStatus.FileSystem.LastFormattedAt = lastFormatted
	}

	// Update device path
	newStatus.DevPath = devPath

	if !reflect.DeepEqual(oldStatus, newStatus) {
		logrus.Infof("Update existing block device status %s", device.Name)
		device.Status.DeviceStatus = newStatus
	}
	// Only disk hasn't yet been formatted can be auto-provisioned.
	if needAutoProvision {
		logrus.Infof("Auto provisioning block device %s", device.Name)
		device.Spec.FileSystem.ForceFormatted = true
		device.Spec.FileSystem.Provisioned = true
	}
	return nil
}

// OnBlockDeviceDelete will delete the block devices that belongs to the same parent device
func (c *Controller) OnBlockDeviceDelete(_ string, device *diskv1.BlockDevice) (*diskv1.BlockDevice, error) {

	if !CacheDiskTags.Initialized() {
		return nil, errors.New(errorCacheDiskTagsNotInitialized)
	}

	if device == nil {
		return nil, nil
	}

	bds, err := c.BlockdeviceCache.List(c.Namespace, labels.SelectorFromSet(map[string]string{
		corev1.LabelHostname: c.NodeName,
		ParentDeviceLabel:    device.Name,
	}))
	if err != nil {
		return device, err
	}

	if len(bds) == 0 {
		return nil, nil
	}

	// Remove dangling blockdevice partitions
	for _, bd := range bds {
		if err := c.Blockdevices.Delete(c.Namespace, bd.Name, &metav1.DeleteOptions{}); err != nil {
			return device, err
		}
	}

	// Clean disk from related longhorn node
	node, err := c.Nodes.Get(c.Namespace, c.NodeName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return device, err
	}
	if node == nil {
		logrus.Debugf("node %s is not there. Skip disk deletion from node", c.NodeName)
		return nil, nil
	}
	nodeCpy := node.DeepCopy()
	for _, bd := range bds {
		if _, ok := nodeCpy.Spec.Disks[bd.Name]; !ok {
			logrus.Debugf("disk %s not found in disks of longhorn node %s/%s", bd.Name, c.Namespace, c.NodeName)
			continue
		}
		existingMount := bd.Status.DeviceStatus.FileSystem.MountPoint
		if existingMount != "" {
			if err := utils.UmountDisk(existingMount); err != nil {
				logrus.Warnf("cannot umount disk %s from mount point %s, err: %s", bd.Name, existingMount, err.Error())
			}
		}
		delete(nodeCpy.Spec.Disks, bd.Name)
	}
	if _, err := c.Nodes.Update(nodeCpy); err != nil {
		return device, err
	}

	CacheDiskTags.DeleteDiskTags(device.Name)

	return nil, nil
}

func resolvePersistentDevPath(device *diskv1.BlockDevice) (string, error) {
	switch device.Status.DeviceStatus.Details.DeviceType {
	case diskv1.DeviceTypeDisk:
		// Disk naming priority.
		// #1 WWN
		// #2 filesystem UUID (UUID)
		// #3 partition table UUID (PTUUID)
		// #4 PtUUID as UUID to query disk info
		//    (NDM might reuse PtUUID as UUID to format a disk)
		if wwn := device.Status.DeviceStatus.Details.WWN; valueExists(wwn) {
			if device.Status.DeviceStatus.Details.StorageController == string(diskv1.StorageControllerNVMe) {
				return filepath.EvalSymlinks("/dev/disk/by-id/nvme-" + wwn)
			}
			return filepath.EvalSymlinks("/dev/disk/by-id/wwn-" + wwn)
		}
		if fsUUID := device.Status.DeviceStatus.Details.UUID; valueExists(fsUUID) {
			path, err := filepath.EvalSymlinks("/dev/disk/by-uuid/" + fsUUID)
			if err == nil {
				return path, nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		}

		if ptUUID := device.Status.DeviceStatus.Details.PtUUID; valueExists(ptUUID) {
			path, err := block.GetDevPathByPTUUID(ptUUID)
			if err != nil {
				return "", err
			}
			if path != "" {
				return path, nil
			}
			return filepath.EvalSymlinks("/dev/disk/by-uuid/" + ptUUID)
		}
		return "", fmt.Errorf("WWN/UUID/PTUUID was not found on device %s", device.Name)
	case diskv1.DeviceTypePart:
		partUUID := device.Status.DeviceStatus.Details.PartUUID
		if partUUID == "" {
			return "", fmt.Errorf("PARTUUID was not found on device %s", device.Name)
		}
		return filepath.EvalSymlinks("/dev/disk/by-partuuid/" + partUUID)
	default:
		return "", nil
	}
}

func extraDiskMountPoint(bd *diskv1.BlockDevice) string {
	// DEPRECATED: only for backward compatibility
	if bd.Spec.FileSystem.MountPoint != "" {
		return bd.Spec.FileSystem.MountPoint
	}

	return fmt.Sprintf("/var/lib/harvester/extra-disks/%s", bd.Name)
}

func needUpdateMountPoint(bd *diskv1.BlockDevice, filesystem *block.FileSystemInfo) NeedMountUpdateOP {
	if filesystem == nil {
		logrus.Debugf("Filesystem is not ready, skip the mount operation")
		return NeedMountUpdateNoOp
	}

	logrus.Debugf("Checking mount operation with FS.Provisioned %v, FS.Mountpoint %s", bd.Spec.FileSystem.Provisioned, filesystem.MountPoint)
	if bd.Spec.FileSystem.Provisioned {
		if filesystem.MountPoint == "" {
			return NeedMountUpdateMount
		}
		if filesystem.MountPoint == extraDiskMountPoint(bd) {
			logrus.Debugf("Already mounted, return no-op")
			return NeedMountUpdateNoOp
		}
		return NeedMountUpdateUnmount | NeedMountUpdateMount
	}
	if filesystem.MountPoint != "" {
		return NeedMountUpdateUnmount
	}
	return NeedMountUpdateNoOp
}

// jitterEnqueueDelay returns a random duration between 3 to 7.
func jitterEnqueueDelay() time.Duration {
	enqueueDelay := 5
	randNum, err := gocommon.GenRandNumber(2)
	if err != nil {
		logrus.Errorf("Failed to generate random number, set randnumber to `0`: %v", err)
	}
	return time.Duration(int(randNum)+enqueueDelay) * time.Second
}

func convertMountStr(mountOP NeedMountUpdateOP) string {
	switch mountOP {
	case NeedMountUpdateNoOp:
		return "No-Op"
	case NeedMountUpdateMount:
		return "Mount"
	case NeedMountUpdateUnmount:
		return "Unmount"
	}
	return "Unknown OP"
}

func convertFSInfoToString(fsInfo *block.FileSystemInfo) string {
	// means this device is not mounted
	if fsInfo.MountPoint == "" {
		return "device is not mounted"
	}
	return fmt.Sprintf("mountpoint: %s, fsType: %s", fsInfo.MountPoint, fsInfo.Type)
}

func removeUnNeeded[T string | int](x []T, y []T) []T {
	result := make([]T, 0)
	for _, item := range x {
		if !slices.Contains(y, item) {
			result = append(result, item)
		}
	}
	return result
}
