/*
CSI Driver for Linstor
Copyright © 2018 LINBIT USA, LLC

This program is free software; you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation; either version 2 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program; if not, see <http://www.gnu.org/licenses/>.
*/

package driver

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/haySwim/data"
	"github.com/piraeusdatastore/linstor-csi/pkg/client"
	"github.com/piraeusdatastore/linstor-csi/pkg/volume"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Version is set via ldflags configued in the Makefile.
var Version = "UNKNOWN"

// Driver fullfils CSI controller, node, and indentity server interfaces.
type Driver struct {
	Storage       volume.CreateDeleter
	Assignments   volume.AttacherDettacher
	Mounter       volume.Mounter
	Snapshots     volume.SnapshotCreateDeleter
	VolumeStatter volume.VolumeStatter
	Expander      volume.Expander
	NodeInformer  volume.NodeInformer
	srv           *grpc.Server
	log           *logrus.Entry
	version       string
	// name distingushes the driver from other drivers and is used to mark
	// volumes so that volumes provisioned by another driver are not interfered with.
	name string
	// endpoint is the socket over which all CSI calls are requested and responded to.
	endpoint string
	// nodeID is the hostname of the node where this plugin is running locally.
	nodeID string
}

// NewDriver builds up a driver.
func NewDriver(options ...func(*Driver) error) (*Driver, error) {
	// Set up default noop-ish storage backend.
	mockStorage := &client.MockStorage{}

	d := &Driver{
		name:          "linstor.csi.linbit.com",
		version:       Version,
		nodeID:        "localhost",
		Storage:       mockStorage,
		Assignments:   mockStorage,
		Mounter:       mockStorage,
		Snapshots:     mockStorage,
		Expander:      mockStorage,
		VolumeStatter: mockStorage,
		NodeInformer:  mockStorage,
		log:           logrus.NewEntry(logrus.New()),
	}

	d.log.Logger.SetOutput(ioutil.Discard)
	d.log.Logger.SetFormatter(&logrus.TextFormatter{})

	d.endpoint = fmt.Sprintf("unix:///var/lib/kubelet/plugins/%s/csi.sock", d.name)

	// Run options functions.
	for _, opt := range options {
		err := opt(d)
		if err != nil {
			return nil, err
		}
	}

	// Add in fields that may have been configured above.
	d.log = d.log.WithFields(logrus.Fields{
		"linstorCSIComponent": "driver",
		"version":             d.version,
		"provisioner":         d.name,
		"nodeID":              d.nodeID,
	})

	return d, nil
}

// Storage configures the volume service backend.
func Storage(s volume.CreateDeleter) func(*Driver) error {
	return func(d *Driver) error {
		d.Storage = s
		return nil
	}
}

func Expander(s volume.Expander) func(*Driver) error {
	return func(d *Driver) error {
		d.Expander = s
		return nil
	}
}

// Assignments configures the volume attachment service backend.
func Assignments(a volume.AttacherDettacher) func(*Driver) error {
	return func(d *Driver) error {
		d.Assignments = a
		return nil
	}
}

// Snapshots configures the volume snapshot service backend.
func Snapshots(s volume.SnapshotCreateDeleter) func(*Driver) error {
	return func(d *Driver) error {
		d.Snapshots = s
		return nil
	}
}

// Mounter configures the volume mounting service backend.
func Mounter(m volume.Mounter) func(*Driver) error {
	return func(d *Driver) error {
		d.Mounter = m
		return nil
	}
}

// VolumeStatter configures the volume stats service backend.
func VolumeStatter(s volume.VolumeStatter) func(*Driver) error {
	return func(d *Driver) error {
		d.VolumeStatter = s
		return nil
	}
}

func NodeInformer(n volume.NodeInformer) func(*Driver) error {
	return func(d *Driver) error {
		d.NodeInformer = n
		return nil
	}
}

// NodeID configures the driver node ID.
func NodeID(nodeID string) func(*Driver) error {
	return func(d *Driver) error {
		d.nodeID = nodeID
		return nil
	}
}

// Endpoint configures the driver name.
func Endpoint(ep string) func(*Driver) error {
	return func(d *Driver) error {
		d.endpoint = ep
		return nil
	}
}

// Name configures the driver name.
func Name(name string) func(*Driver) error {
	return func(d *Driver) error {
		d.name = name
		return nil
	}
}

// LogOut sets the driver to write logs to the provided io.writer
// instead of discarding logs.
func LogOut(out io.Writer) func(*Driver) error {
	return func(d *Driver) error {
		d.log.Logger.SetOutput(out)
		return nil
	}
}

// LogFmt sets the format of the log outpout via the provided logrus.Formatter.
func LogFmt(fmt logrus.Formatter) func(*Driver) error {
	return func(d *Driver) error {
		d.log.Logger.SetFormatter(fmt)
		return nil
	}
}

// LogLevel sets the logging intensity. Debug additionally reports the function
// from which the logger was called.
func LogLevel(s string) func(*Driver) error {
	return func(d *Driver) error {
		level, err := logrus.ParseLevel(s)
		if err != nil {
			return fmt.Errorf("unable to use %s as a logging level: %v", s, err)
		}

		d.log.Logger.SetLevel(level)

		// logs function name from which the logger was called
		if level == logrus.DebugLevel {
			d.log.Logger.SetReportCaller(true)
		}
		return nil
	}
}

// GetPluginInfo https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#getplugininfo
func (d Driver) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          d.name,
		VendorVersion: d.version,
	}, nil
}

// GetPluginCapabilities https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#getplugincapabilities
func (d Driver) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{
					Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
				}}},
			{Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{
					Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
				}}},
		},
	}, nil
}

// Probe https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#probe
func (d Driver) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

// NodeStageVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#nodestagevolume
func (d Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "")
}

// NodeUnstageVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#nodeunstagevolume
func (d Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "")
}

// NodePublishVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#nodepublishvolume
func (d Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return &csi.NodePublishVolumeResponse{}, missingAttr("NodePublishVolume", req.GetVolumeId(), "VolumeId")
	}

	if req.GetTargetPath() == "" {
		return &csi.NodePublishVolumeResponse{}, missingAttr("NodePublishVolume", req.GetVolumeId(), "TargetPath")
	}

	if req.GetVolumeCapability() == nil {
		return &csi.NodePublishVolumeResponse{}, missingAttr("NodePublishVolume", req.GetVolumeId(), "VolumeCapability slice")
	}

	// Don't try to publish volumes in ROX configurations without the "ro" option.
	// You might think this is something ControllerPublishVolume could do already. You are wrong. The Readonly flag
	// passed to ControllerPublishVolume is *always* false i.e. completely useless.
	// The Readonly flag passed here is the one set in pod specs as spec.volumes[].persistentVolumeClaim.readOnly
	// See: https://github.com/kubernetes/kubernetes/issues/70505
	if req.GetVolumeCapability().GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY && !req.GetReadonly() {
		return nil, status.Errorf(codes.InvalidArgument, "NodePublishVolume failed for %s: access mode requires 'persistentVolumeClaim.readOnly' to be true", req.GetVolumeId())
	}

	var mntOpts = make([]string, 0)
	var fsType string

	if block := req.GetVolumeCapability().GetBlock(); block != nil {
		mntOpts = []string{"bind"}
	}

	if mnt := req.GetVolumeCapability().GetMount(); mnt != nil {
		mntOpts = mnt.MountFlags
		fsType = "ext4"
		if mnt.FsType != "" {
			fsType = mnt.FsType
		}
	}

	// Retrieve device path from storage backend.
	existingVolume, err := d.Storage.FindByID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume failed for %s: %v", req.GetVolumeId(), err)
	}

	if existingVolume == nil {
		return nil, status.Errorf(codes.NotFound, "NodePublishVolume failed for %s: volume not found", req.GetVolumeId())
	}

	assignment, err := d.Assignments.GetAssignmentOnNode(ctx, existingVolume, d.nodeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume failed for %s: %v", req.GetVolumeId(), err)
	}

	err = d.Mounter.Mount(existingVolume, assignment.Path, req.GetTargetPath(), fsType, req.GetReadonly(), mntOpts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume failed for %s: %v", req.GetVolumeId(), err)
	}

	// Runs post-mount xfs_io if PostMountXfsOpts is configured
	if fsType == "xfs" {

		xfsIoParam := existingVolume.Parameters["PostMountXfsOpts"]

		if xfsIoParam != "" {

			d.log.WithFields(logrus.Fields{
				"XFS_IO":     xfsIoParam,
				"FSType":     fsType,
				"targetPath": req.GetTargetPath(),
			}).Debug("Post-mount XXXFS_io")

			// xfs_io -c "extsize 2m" /mnt/dax
			command := exec.Command("xfs_io", "-c", xfsIoParam, req.GetTargetPath())
			_, err := command.CombinedOutput()
			if err != nil {
				return nil, status.Errorf(codes.Internal, "NodePublishVolume failed for %s: %v", req.GetVolumeId(), err)
			}
		}
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#nodeunpublishvolume
func (d Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, missingAttr("NodeUnpublishVolume", req.GetVolumeId(), "VolumeId")
	}

	if req.GetTargetPath() == "" {
		return nil, missingAttr("NodeUnpublishVolume", req.GetVolumeId(), "TargetPath")
	}

	err := d.Mounter.Unmount(req.GetTargetPath())
	if err != nil {
		return nil, err
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#nodegetvolumestats
func (d Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, missingAttr("NodeGetVolumeStats", req.GetVolumeId(), "VolumeId")
	}

	if req.GetVolumePath() == "" {
		return nil, missingAttr("NodeGetVolumeStats", req.GetVolumeId(), "VolumeId")
	}

	volume, err := d.Storage.FindByID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeGetVolumeStats failed for %s: failed to check if volume exists: %v", req.GetVolumeId(), err)
	}

	if volume == nil {
		return nil, status.Errorf(codes.NotFound, "NodeGetVolumeStats failed for %s: volume does not exist", req.GetVolumeId())
	}

	notMounted, err := d.Mounter.IsNotMountPoint(req.GetVolumePath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeGetVolumeStats failed for %s: failed to check if path %v is mounted: %v", req.GetVolumeId(), req.GetVolumePath(), err)
	}

	if notMounted {
		return nil, status.Errorf(codes.NotFound, "NodeGetVolumeStats failed for %s: path %v is not mounted", req.GetVolumeId(), req.GetVolumePath())
	}

	stats, err := d.VolumeStatter.GetVolumeStats(req.GetVolumePath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeGetVolumeStats failed for %s: failed to get stats: %v", req.GetVolumeId(), err)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: stats.AvailableBytes,
				Total:     stats.TotalBytes,
				Used:      stats.UsedBytes,
				Unit:      csi.VolumeUsage_BYTES,
			},
			{
				Available: stats.AvailableInodes,
				Total:     stats.TotalInodes,
				Used:      stats.UsedInodes,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
	}, nil
}

// NodeGetCapabilities https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#nodegetcapabilities
func (d Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			&csi.NodeServiceCapability{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
			&csi.NodeServiceCapability{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
		},
	}, nil
}

// NodeGetInfo https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#nodegetinfo
func (d Driver) NodeGetInfo(ctx context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	topology, err := d.NodeInformer.GetNodeTopologies(ctx, d.nodeID)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve node topology: %w", err)
	}

	return &csi.NodeGetInfoResponse{
		NodeId:             d.nodeID,
		MaxVolumesPerNode:  1048576, // DRBD volumes per node limit.
		AccessibleTopology: topology,
	}, nil
}

// CreateVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#createvolume
func (d Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return &csi.CreateVolumeResponse{}, missingAttr("CreateVolume", req.GetName(), "Name")
	}
	if req.GetVolumeCapabilities() == nil || len(req.GetVolumeCapabilities()) == 0 {
		return &csi.CreateVolumeResponse{}, missingAttr("ValidateVolumeCapabilities", req.GetName(), "VolumeCapabilities")
	}

	// Determine how much storage we need to actually allocate for a given number
	// of bytes.
	requiredKiB, err := d.Storage.AllocationSizeKiB(req.GetCapacityRange().GetRequiredBytes(), req.GetCapacityRange().GetLimitBytes())
	if err != nil {
		return &csi.CreateVolumeResponse{}, status.Errorf(
			codes.Internal, "CreateVolume failed for %s: %v", req.Name, err)
	}
	volumeSize := data.NewKibiByte(data.KiB * data.ByteSize(requiredKiB))

	// Handle case were a volume of the same name is already present.
	existingVolume, err := d.Storage.FindByName(ctx, req.Name)
	if err != nil {
		return &csi.CreateVolumeResponse{}, status.Errorf(
			codes.Internal, "CreateVolume failed for %s: %v", req.Name, err)
	}

	if existingVolume != nil {
		d.log.WithFields(logrus.Fields{
			"requestedVolumeName": req.GetName(),
			"existingVolume":      fmt.Sprintf("%+v", existingVolume),
		}).Info("volume already present")

		if existingVolume.CreatedBy != d.name {
			return &csi.CreateVolumeResponse{}, status.Errorf(codes.AlreadyExists,
				"CreateVolume failed for %s: volume already present and wasn't created by %s",
				req.GetName(), d.name)
		}

		if existingVolume.SizeBytes != int64(volumeSize.InclusiveBytes()) {
			return &csi.CreateVolumeResponse{}, status.Errorf(codes.AlreadyExists,
				"CreateVolume failed for %s: volume already present, created by %s, but size differs (existing: %d, wanted: %d",
				req.GetName(), d.name, existingVolume.SizeBytes, int64(volumeSize.InclusiveBytes()))
		}

		d.log.WithFields(logrus.Fields{
			"requestedVolumeName": req.GetName(),
			"volumeSize":          volumeSize,
			"expectedCreatedBy":   d.name,
			"existingVolume":      fmt.Sprintf("%+v", existingVolume),
		}).Info("volume already present, but matches request")

		d.log.Debug("check if source snapshot exists")

		snapId := d.Snapshots.CompatibleSnapshotId(snapshotForVolumeName(req.GetName()))
		leftoverSnap, err := d.Snapshots.FindSnapByID(ctx, snapId)
		if err != nil {
			return &csi.CreateVolumeResponse{}, status.Errorf(codes.Internal, "failed to check on potential left-over source snapshot: %v", err)
		}

		if leftoverSnap != nil {
			d.log.Debug("found left-over source snapshot, removing...")

			err := d.Snapshots.SnapDelete(ctx, leftoverSnap)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to delete source snapshot for volume '%s': %v", existingVolume.ID, err)
			}
		}

		topos, err := d.Storage.AccessibleTopologies(ctx, existingVolume)
		if err != nil {
			return &csi.CreateVolumeResponse{}, status.Errorf(
				codes.Internal, "CreateVolume failed for %s: unable to determine volume topology: %v",
				req.GetName(), err)
		}

		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:           existingVolume.ID,
				CapacityBytes:      existingVolume.SizeBytes,
				ContentSource:      req.GetVolumeContentSource(),
				AccessibleTopology: topos,
			}}, nil
	}

	return d.createNewVolume(ctx, req)
}

// DeleteVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#deletevolume
func (d Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, missingAttr("DeleteVolume", req.GetVolumeId(), "VolumeId")
	}

	existingVolume, err := d.Storage.FindByID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	if existingVolume == nil {
		// Volume doesn't exist, and that's the point of this call after all.
		return &csi.DeleteVolumeResponse{}, nil
	}
	d.log.WithFields(logrus.Fields{
		"existingVolume": fmt.Sprintf("%+v", existingVolume),
	}).Debug("found existing volume")

	snaps, err := d.Snapshots.FindSnapsBySource(ctx, existingVolume, 0, 0)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check for existing snapshots on volume: %v", err)
	}

	if len(snaps) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "resource has associated snapshots: %v", err)
	}

	if err := d.Storage.Delete(ctx, existingVolume); err != nil {
		return nil, err
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#controllerpublishvolume
func (d Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, missingAttr("ControllerPublishVolume", req.GetVolumeId(), "VolumeId")
	}
	if req.GetNodeId() == "" {
		return nil, missingAttr("ControllerPublishVolume", req.GetVolumeId(), "NodeId")
	}
	if req.GetVolumeCapability() == nil {
		return nil, missingAttr("ControllerPublishVolume", req.GetVolumeId(), "VolumeCapability")
	}

	// Don't try to assign volumes that don't exist.
	existingVolume, err := d.Storage.FindByID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"ControllerPublishVolume failed for %s: %v", req.GetVolumeId(), err)
	}
	if existingVolume == nil {
		return nil, status.Errorf(codes.NotFound,
			"ControllerPublishVolume failed for %s: volume not present in storage backend",
			req.GetVolumeId())
	}
	d.log.WithFields(logrus.Fields{
		"existingVolume": fmt.Sprintf("%+v", existingVolume),
	}).Debug("found existing volume")

	// Or have had their attributes changed.
	if existingVolume.Readonly != req.GetReadonly() {
		return nil, status.Errorf(codes.AlreadyExists,
			"ControllerPublishVolume failed for %s: volume attributes changes for already existing volume",
			req.GetVolumeId())
	}

	// Don't even attempt to put it on nodes that aren't avaible.
	if err := d.Assignments.NodeAvailable(ctx, req.GetNodeId()); err != nil {
		return nil, status.Errorf(codes.NotFound,
			"ControllerPublishVolume failed for %s on node %s: %v", req.GetVolumeId(), req.GetNodeId(), err)
	}

	err = d.Assignments.Attach(ctx, existingVolume, req.GetNodeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"ControllerPublishVolume failed for %s: %v", req.GetVolumeId(), err)
	}

	return &csi.ControllerPublishVolumeResponse{}, nil
}

// ControllerUnpublishVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#controllerunpublishvolume
func (d Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, missingAttr("ControllerUnpublishVolume", req.GetVolumeId(), "VolumeId")
	}
	if req.GetNodeId() == "" {
		return nil, missingAttr("ControllerUnpublishVolume", req.GetNodeId(), "NodeId")
	}

	vol, err := d.Storage.FindByID(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"ControllerUnpublishVolume failed for %s: %v", req.GetVolumeId(), err)
	}
	// If it's not there, it's as detached as we can make it, right?
	if vol == nil {
		d.log.WithFields(logrus.Fields{
			"volumeId": req.GetVolumeId(),
			"nodeId":   req.GetNodeId(),
		}).Info("volume to be unpublished was not found in storage backend")
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	d.log.WithFields(logrus.Fields{
		"volume": fmt.Sprintf("%+v", vol),
		"nodeId": req.GetNodeId(),
	}).Debug("found existing volume")

	if err := d.Assignments.Detach(ctx, vol, req.GetNodeId()); err != nil {
		return nil, status.Errorf(codes.Internal,
			"ControllerpublishVolume failed for %s: %v", req.GetVolumeId(), err)
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#validatevolumecapabilities
func (d Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, missingAttr("ValidateVolumeCapabilities", req.GetVolumeId(), "volumeId")
	}

	if req.GetVolumeCapabilities() == nil {
		return nil, missingAttr("ValidateVolumeCapabilities", req.GetVolumeId(), "VolumeCapabilities")
	}

	existingVolume, err := d.Storage.FindByID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"ValidateVolumeCapabilities failed for %s: %v", req.GetVolumeId(), err)
	}
	if existingVolume == nil {
		return nil, status.Errorf(codes.NotFound,
			"ValidateVolumeCapabilities failed for %s: volume not present in storage backend", req.GetVolumeId())
	}
	d.log.WithFields(logrus.Fields{
		"existingVolume": fmt.Sprintf("%+v", existingVolume),
	}).Debug("found existing volume")

	for _, requested := range req.VolumeCapabilities {
		requestedMode := requested.GetAccessMode().GetMode()
		if requestedMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER && requestedMode != csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
			return nil, status.Errorf(codes.InvalidArgument, "ValidateVolumeCapabilities failed for %s: volumes support only RWO and ROX mode", req.GetVolumeId())
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: []*csi.VolumeCapability{
				// Tell CO we can provision RWO and ROX mount volumes.
				{AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{}},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
				{AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{}},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY}},
				// Tell CO we can provision RWO and ROX block volumes.
				{AccessType: &csi.VolumeCapability_Block{
					Block: &csi.VolumeCapability_BlockVolume{}},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
				{AccessType: &csi.VolumeCapability_Block{
					Block: &csi.VolumeCapability_BlockVolume{}},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY}},
			},
		}}, nil
}

// ListVolumes https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#listvolumes
func (d Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {

	volumes, err := d.Storage.ListAll(ctx)
	if err != nil {
		return &csi.ListVolumesResponse{}, status.Errorf(codes.Aborted, "ListVolumes failed: %v", err)
	}

	start, err := parseAsInt(req.GetStartingToken())
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "ListVolumes failed for: %v", err)
	}

	if start > len(volumes) {
		return nil, status.Errorf(codes.Aborted,
			"ListVolumes failed: requsted more volumes (%d) then the total number of volumes (%d)",
			start, len(volumes))
	}

	// Handle pagination.
	var (
		end       int32
		nextToken string
	)
	totalVols := int32(len(volumes))
	if req.GetMaxEntries() == 0 || (totalVols <= req.GetMaxEntries()) {
		end = totalVols
	} else {
		end = req.GetMaxEntries()
		nextToken = strconv.Itoa(int(end))
	}

	volumes = volumes[start:end]

	// Build up entries list from paginated volume slice.
	var entries = make([]*csi.ListVolumesResponse_Entry, len(volumes))
	for i, vol := range volumes {
		topos, err := d.Storage.AccessibleTopologies(ctx, vol)
		if err != nil {
			return nil, status.Errorf(
				codes.Internal, "ListVolumes failed for %s: unable to determine volume topology: %v",
				vol.Name, err)
		}

		entries[i] = &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:           vol.ID,
				CapacityBytes:      vol.SizeBytes,
				AccessibleTopology: topos,
			}}
	}

	return &csi.ListVolumesResponse{NextToken: nextToken, Entries: entries}, nil
}

// GetCapacity https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#getcapacity
func (d Driver) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	bytes, err := d.Storage.CapacityBytes(ctx, req.GetParameters())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &csi.GetCapacityResponse{AvailableCapacity: bytes}, nil
}

// ControllerGetCapabilities https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#controllergetcapabilities
func (d Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			// Tell the CO we can create and delete volumes.
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
				}}},
			// Tell the CO we can make volumes available on remote nodes.
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
				}}},
			// Tell the CO we can list volumes.
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
				}}},
			// Tell the CO we can create and delete snapshots.
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
				}}},
			// Tell the CO we can create clones of volumes.
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
				}}},
			// Tell the CO we can list snapshots.
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
				}}},
			// Tell the CO we can query our storage space.
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_GET_CAPACITY,
				}}},
			// Tell the CO we support readonly volumes.
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_PUBLISH_READONLY,
				}}},
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
				}}},
		},
	}, nil
}

// CreateSnapshot https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#createsnapshot
func (d Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if req.GetSourceVolumeId() == "" {
		return nil, missingAttr("CreateSnapshot", req.GetSourceVolumeId(), "SourceVolumeId")
	}
	if req.GetName() == "" {
		return nil, missingAttr("CreateSnapshot", req.GetName(), "Name")
	}

	id := d.Snapshots.CompatibleSnapshotId(req.GetName())

	d.log.WithField("snapshot id", id).Debug("using snapshot id")

	existingSnap, err := d.Snapshots.FindSnapByID(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check for existing snapshot: %v", err)
	}

	if existingSnap != nil {
		// Needed for idempotency.
		d.log.WithFields(logrus.Fields{
			"requestedSnapshotName":         req.GetName(),
			"requestedSnapshotSourceVolume": req.GetSourceVolumeId(),
			"existingSnapshot":              fmt.Sprintf("%+v", existingSnap),
		}).Debug("found existing snapshot")

		if existingSnap.GetSourceVolumeId() == req.GetSourceVolumeId() {
			return &csi.CreateSnapshotResponse{Snapshot: existingSnap}, nil
		}
		return nil, status.Errorf(codes.AlreadyExists, "can't use %q for snapshot name for volume %q, snapshot name is in use for volume %q",
			req.GetName(), req.GetSourceVolumeId(), existingSnap.GetSourceVolumeId())
	}

	info, err := d.Storage.FindByID(ctx, req.GetSourceVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "could not find source for snapshot: %v", err)
	}

	if info == nil {
		return nil, status.Errorf(codes.NotFound, "source '%s' for snapshot does not exist", req.GetSourceVolumeId())
	}

	snap, err := d.Snapshots.SnapCreate(ctx, id, info)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create snapshot: %v", err)
	}

	return &csi.CreateSnapshotResponse{Snapshot: snap}, nil
}

// DeleteSnapshot https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#deletesnapshot
func (d Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, missingAttr("DeleteSnapshot", req.GetSnapshotId(), "SnapshotId")
	}

	snap, err := d.Snapshots.FindSnapByID(ctx, req.GetSnapshotId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to find snapshot %s: %v",
			req.GetSnapshotId(), err)
	}
	if snap == nil {
		d.log.WithFields(logrus.Fields{
			"snapshotId": req.GetSnapshotId(),
		}).Info("unable to find snapshot, it may already be deleted")
		return &csi.DeleteSnapshotResponse{}, nil
	}

	if err := d.Snapshots.SnapDelete(ctx, snap); err != nil {
		return nil, status.Errorf(codes.Internal, "unable to delete snapshot %s: %v",
			req.GetSnapshotId(), err)
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#listsnapshots
func (d Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	limit := int(req.GetMaxEntries())
	start, err := parseAsInt(req.GetStartingToken())
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "Invalid starting token: %v", err)
	}

	var snapshots []*csi.Snapshot
	switch {
	// Handle case where a single snapshot is requested.
	case req.GetSnapshotId() != "":
		snap, err := d.Snapshots.FindSnapByID(ctx, req.GetSnapshotId())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to list snapshots: %v", err)
		}

		if snap == nil {
			return &csi.ListSnapshotsResponse{}, nil
		}

		d.log.WithFields(logrus.Fields{
			"requestedSnapshot": req.GetSnapshotId(),
			"napshot":           fmt.Sprintf("%+v", snap),
		}).Debug("found single snapshot")
		snapshots = []*csi.Snapshot{snap}

		// Handle case where a single volumes snapshots are requested.
	case req.GetSourceVolumeId() != "":
		info, err := d.Storage.FindByID(ctx, req.GetSourceVolumeId())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to list snapshots for volume '%s': %v", req.GetSourceVolumeId(), err)
		}

		if info == nil {
			return &csi.ListSnapshotsResponse{}, nil
		}

		snaps, err := d.Snapshots.FindSnapsBySource(ctx, info, start, limit)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to list snapshots: %v", err)
		}

		snapshots = snaps

		// Regular list of all snapshots.
	default:
		snaps, err := d.Snapshots.ListSnaps(ctx, start, limit)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to list snapshots: %v", err)
		}
		snapshots = snaps
	}

	nextToken := ""
	if limit > 0 && len(snapshots) == limit {
		nextToken = fmt.Sprintf("%d", start+limit)
	}

	entries := make([]*csi.ListSnapshotsResponse_Entry, len(snapshots))
	for i, snap := range snapshots {
		entries[i] = &csi.ListSnapshotsResponse_Entry{Snapshot: snap}
	}

	return &csi.ListSnapshotsResponse{Entries: entries, NextToken: nextToken}, nil
}

// NodeExpandVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#nodeexpandvolume
func (d Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	d.log.WithFields(logrus.Fields{
		"NodeExpandVolume": fmt.Sprintf("%+v", req),
	}).Debug("Node expand volume")

	if req.GetVolumeId() == "" {
		return nil, missingAttr("NodeExpandVolume", req.GetVolumeId(), "VolumeId")
	}

	if req.GetVolumePath() == "" {
		return nil, missingAttr("NodeExpandVolume", req.GetVolumeId(), "TargetPath")
	}

	// Retrieve device path from storage backend.
	existingVolume, err := d.Storage.FindByID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeExpandVolume - get resource-definitions %s failed: %v", req.GetVolumeId(), err)
	}
	if existingVolume == nil {
		// Volume doesn't exist, and that's the point of this call after all.
		return nil, status.Errorf(codes.Internal,
			"NodeExpandVolume - resource-definitions %s not found", req.GetVolumeId())
	}

	assignment, err := d.Assignments.GetAssignmentOnNode(ctx, existingVolume, d.nodeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeExpandVolume - get assignment failed for volume %s node: %s: %v", req.GetVolumeId(), d.nodeID, err)
	}

	err = d.Expander.NodeExpand(assignment.Path, req.GetVolumePath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeExpandVolume - expand volume fail source %s target %s, err: %v.",
			assignment.Path, req.GetVolumePath(), err)
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

// ControllerExpandVolume https://github.com/container-storage-interface/spec/blob/v1.2.0/spec.md#controllerexpandvolume
func (d Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, missingAttr("ControllerExpandVolume", req.GetVolumeId(), "VolumeId")
	}

	existingVolume, err := d.Storage.FindByID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ControllerExpandVolume - get resource-definitions %s failed: %v", req.GetVolumeId(), err)
	}
	if existingVolume == nil {
		// Volume doesn't exist, and that's the point of this call after all.
		return nil, status.Errorf(codes.Internal,
			"ControllerExpandVolume - resource-definitions %s not found", req.GetVolumeId())
	}
	d.log.WithFields(logrus.Fields{
		"existingVolume": fmt.Sprintf("%+v", existingVolume),
	}).Debug("found existing volume")

	requiredKiB, err := d.Storage.AllocationSizeKiB(req.CapacityRange.GetRequiredBytes(), req.CapacityRange.GetLimitBytes())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ControllerExpandVolume - expand volume failed for volume id %s: %v", req.GetVolumeId(), err)
	}
	volumeSize := data.NewKibiByte(data.KiB * data.ByteSize(requiredKiB))
	existingVolume.SizeBytes = int64(volumeSize.InclusiveBytes())

	d.log.WithFields(logrus.Fields{
		"ControllerExpandVolume": fmt.Sprintf("%+v", req),
		"Size":                   volumeSize,
	}).Debug("controller expand volume")

	err = d.Expander.ControllerExpand(ctx, existingVolume)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"ControllerExpandVolume - expand volume failed for %v: %v", existingVolume, err)
	}

	isBlockMode := req.GetVolumeCapability().GetBlock() != nil

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes: existingVolume.SizeBytes, NodeExpansionRequired: !isBlockMode}, nil
}

// Run the server.
func (d Driver) Run() error {
	d.log.Debug("Preparing to start server")

	u, err := url.Parse(d.endpoint)
	if err != nil {
		return fmt.Errorf("unable to parse address: %q", err)
	}

	addr := path.Join(u.Host, filepath.FromSlash(u.Path))
	if u.Host == "" {
		addr = filepath.FromSlash(u.Path)
	}

	// csi plugins talk only over unix sockets currently
	if u.Scheme != "unix" {
		return fmt.Errorf("currently only unix domain sockets are supported, have: %s", u.Scheme)
	}
	if err = os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove previously used unix domain socket file %s, error: %v", addr, err)
	}

	listener, err := net.Listen(u.Scheme, addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	//type UnaryServerInterceptor func(ctx context.Context, req interface{}, info *UnaryServerInfo, handler UnaryHandler) (resp interface{}, err error)
	errHandler := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)

		if err == nil {
			d.log.WithFields(logrus.Fields{
				"method": info.FullMethod,
				"req":    fmt.Sprintf("%+v", req),
				"resp":   fmt.Sprintf("%+v", resp),
			}).Debug("method called")
		} else {
			d.log.WithFields(logrus.Fields{
				"method": info.FullMethod,
				"req":    fmt.Sprintf("%+v", req),
				"resp":   fmt.Sprintf("%+v", resp),
			}).WithError(err).Error("method failed")
		}

		return resp, err
	}

	d.srv = grpc.NewServer(grpc.UnaryInterceptor(errHandler))
	csi.RegisterIdentityServer(d.srv, d)
	csi.RegisterControllerServer(d.srv, d)
	csi.RegisterNodeServer(d.srv, d)

	d.log.WithFields(logrus.Fields{
		"address": addr,
	}).Info("server started")
	return d.srv.Serve(listener)
}

// Stop the server.
func (d Driver) Stop() error {
	d.srv.GracefulStop()
	return nil
}

func (d Driver) createNewVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	logger := d.log.WithFields(logrus.Fields{
		"request": req.Name,
	})

	requiredKiB, err := d.Storage.AllocationSizeKiB(req.CapacityRange.GetRequiredBytes(), req.CapacityRange.GetLimitBytes())
	if err != nil {
		return &csi.CreateVolumeResponse{}, status.Errorf(codes.Internal, "CreateVolume failed for %s: %v", req.Name, err)
	}

	volumeSize := data.NewKibiByte(data.KiB * data.ByteSize(requiredKiB))

	// Volume id is currently filled in when the volume is created.
	vol := &volume.Info{
		Name:         req.GetName(),
		SizeBytes:    int64(volumeSize.InclusiveBytes()),
		CreatedBy:    d.name,
		CreationTime: time.Now(),
		Parameters:   make(map[string]string),
	}
	for k, v := range req.Parameters {
		vol.Parameters[k] = v
	}
	logger.WithFields(logrus.Fields{
		"newVolume": fmt.Sprintf("%+v", vol),
		"size":      volumeSize,
	}).Debug("creating new volume")

	// We're cloning from a volume or snapshot.
	if req.GetVolumeContentSource() != nil {
		switch {
		case req.GetVolumeContentSource().GetSnapshot() != nil:
			snapshotID := req.GetVolumeContentSource().GetSnapshot().GetSnapshotId()
			if snapshotID == "" {
				return &csi.CreateVolumeResponse{}, status.Errorf(codes.InvalidArgument,
					"CreateVolume failed for %s: empty snapshotId", req.GetName())
			}
			logger.Debugf("pre-populate volume from snapshot: %+v", snapshotID)

			snap, err := d.Snapshots.FindSnapByID(ctx, snapshotID)
			if err != nil {
				return &csi.CreateVolumeResponse{}, status.Errorf(
					codes.Internal, "CreateVolume failed for %s: %v", req.GetName(), err)
			}
			if snap == nil {
				return &csi.CreateVolumeResponse{}, status.Errorf(codes.NotFound,
					"CreateVolume failed for %s: snapshot not found in storage backend", req.GetName())
			}
			sourceVol, err := d.Storage.FindByID(ctx, snap.GetSourceVolumeId())
			if err != nil {
				return &csi.CreateVolumeResponse{}, status.Errorf(codes.Internal,
					"CreateVolume failed for %s: %v", req.GetName(), err)
			}
			if sourceVol == nil {
				return &csi.CreateVolumeResponse{}, status.Errorf(codes.NotFound,
					"CreateVolume failed for %s: source volume not found in storage backend", req.GetName())
			}

			if err := d.Snapshots.VolFromSnap(ctx, snap, vol); err != nil {
				d.failpathDelete(ctx, vol)
				return &csi.CreateVolumeResponse{}, status.Errorf(codes.Internal,
					"CreateVolume failed for %s: %v", req.GetName(), err)
			}
			// We're cloning from a whole volume.
		case req.GetVolumeContentSource().GetVolume() != nil:
			volumeId := req.GetVolumeContentSource().GetVolume().GetVolumeId()
			if volumeId == "" {
				return &csi.CreateVolumeResponse{}, status.Errorf(codes.InvalidArgument,
					"CreateVolume failed for %s: empty volumeId", req.GetName())
			}
			logger.Debugf("pre-populate volume from snapshot: %+v", volumeId)

			sourceVol, err := d.Storage.FindByID(ctx, volumeId)
			if err != nil {
				return &csi.CreateVolumeResponse{}, status.Errorf(codes.Internal,
					"CreateVolume failed for %s: %v", req.GetName(), err)
			}
			if sourceVol == nil {
				return &csi.CreateVolumeResponse{}, status.Errorf(codes.NotFound,
					"CreateVolume failed for %s: source volume not found in storage backend", req.GetName())
			}

			snap, err := d.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: snapshotForVolumeName(req.GetName()), SourceVolumeId: sourceVol.ID})
			if err != nil {
				return nil, status.Errorf(codes.Internal, "CreateVolume failed for %s: failed to create snapshot for source volume: %v", req.GetName(), err)
			}

			if !snap.Snapshot.ReadyToUse {
				return nil, status.Errorf(codes.Internal, "CreateVolume failed for %s: snapshot not ready", req.GetName())
			}

			defer d.Snapshots.SnapDelete(ctx, snap.Snapshot)

			err = d.Snapshots.VolFromSnap(ctx, snap.Snapshot, vol)
			if err != nil {
				d.failpathDelete(ctx, vol)

				return &csi.CreateVolumeResponse{}, status.Errorf(codes.Internal,
					"CreateVolume failed for %s: %v", req.GetName(), err)
			}
		default:
			return &csi.CreateVolumeResponse{}, status.Errorf(codes.InvalidArgument,
				"CreateVolume failed for %s: %v", req.GetName(), err)
		}
		// Regular new volume.
	} else {
		err := d.Storage.Create(ctx, vol, req)
		if err != nil {
			d.failpathDelete(ctx, vol)
			return &csi.CreateVolumeResponse{}, status.Errorf(codes.Internal,
				"CreateVolume failed for %s: %v", req.GetName(), err)
		}
	}

	topos, err := d.Storage.AccessibleTopologies(ctx, vol)
	if err != nil {
		return &csi.CreateVolumeResponse{}, status.Errorf(
			codes.Internal, "CreateVolume failed for %s: unable to determine volume topology: %v",
			req.GetName(), err)
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           vol.ID,
			ContentSource:      req.GetVolumeContentSource(),
			CapacityBytes:      int64(volumeSize.InclusiveBytes()),
			AccessibleTopology: topos,
		}}, nil
}

func missingAttr(methodCall, volumeID, attr string) error {
	if volumeID == "" {
		volumeID = "an unknown volume"
	}
	return status.Errorf(codes.InvalidArgument,
		"%s failed for %s: it requires a %s and none was provided", methodCall, volumeID, attr)
}

func parseAsInt(s string) (int, error) {
	if s == "" {
		return 0, nil
	}

	i, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to parse starting token: %v", err)
	}
	return int(i), nil
}

// Returns the name of the snapshot used to populate data in volume-from-volume scenarios
func snapshotForVolumeName(name string) string {
	return fmt.Sprintf("for-%s", name)
}

// failpathDelete deletes volumes and logs if that fails. Mostly useful
// in error paths where we need to report the original error and not the
// possible error from trying to clean up from that original error.
func (d Driver) failpathDelete(ctx context.Context, vol *volume.Info) {
	if err := d.Storage.Delete(ctx, vol); err != nil {
		d.log.WithFields(logrus.Fields{
			"volume": fmt.Sprintf("%+v", vol),
		}).WithError(err).Error("failed to clean up volume")
	}
}
