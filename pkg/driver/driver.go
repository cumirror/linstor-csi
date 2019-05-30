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
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/LINBIT/linstor-csi/pkg/client"
	"github.com/LINBIT/linstor-csi/pkg/volume"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/haySwim/data"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Version is set via ldflags configued in the Makefile.
var Version = "UNKNOWN"

// Driver fullfils CSI controller, node, and indentity server interfaces.
type Driver struct {
	Storage     volume.CreateDeleter
	Assignments volume.AttacherDettacher
	Mounter     volume.Mounter
	Snapshots   volume.SnapshotCreateDeleter
	srv         *grpc.Server
	log         *logrus.Entry
	version     string
	name        string
	endpoint    string
	nodeID      string
}

// NewDriver build up a driver.
func NewDriver(options ...func(*Driver) error) (*Driver, error) {
	// Set up default noop-ish storage backend.
	mockStorage := &client.MockStorage{}

	d := &Driver{
		name:        "linstor.csi.linbit.com",
		version:     Version,
		nodeID:      "localhost",
		Storage:     mockStorage,
		Assignments: mockStorage,
		Mounter:     mockStorage,
		Snapshots:   mockStorage,
		log:         logrus.NewEntry(logrus.New()),
	}

	d.log.Logger.SetOutput(ioutil.Discard)
	d.log.Logger.SetFormatter(&logrus.TextFormatter{})

	d.endpoint = fmt.Sprintf("unix:///var/lib/kubelet/plugins/%s/csi.sock", d.name)

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

// GetPluginInfo https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#getplugininfo
func (d Driver) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          d.name,
		VendorVersion: d.version,
	}, nil
}

// GetPluginCapabilities https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#getplugincapabilities
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

// Probe https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#probe
func (d Driver) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

// NodeStageVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#nodestagevolume
func (d Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.VolumeId == "" {
		return &csi.NodeStageVolumeResponse{}, missingAttr(
			"NodeStageVolume", req.VolumeId, "VolumeId")
	}

	if req.StagingTargetPath == "" {
		return &csi.NodeStageVolumeResponse{}, missingAttr(
			"NodeStageVolume", req.VolumeId, "StagingTargetPath")
	}

	if req.VolumeCapability == nil {
		return &csi.NodeStageVolumeResponse{}, missingAttr(
			"NodeStageVolume", req.VolumeId, "VolumeCapability slice")
	}

	mnt := req.VolumeCapability.GetMount()
	mntOpts := mnt.MountFlags

	fsType := "ext4"
	if mnt.FsType != "" {
		fsType = mnt.FsType
	}

	existingVolume, err := d.Storage.GetByID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, err
	}

	assignedTo, err := d.Assignments.GetAssignmentOnNode(ctx, existingVolume, d.nodeID)
	if err != nil {
		return nil, err
	}
	if assignedTo == nil {
		return nil, fmt.Errorf("volume %s is not assigned to node %s", existingVolume.ID, d.nodeID)
	}

	err = d.Mounter.Mount(existingVolume, assignedTo.Path, req.StagingTargetPath, fsType, mntOpts)
	if err != nil {
		return nil, err
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#nodeunstagevolume
func (d Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.VolumeId == "" {
		return &csi.NodeUnstageVolumeResponse{}, missingAttr("NodeUnstageVolume", req.VolumeId, "VolumeId")
	}
	if req.StagingTargetPath == "" {
		return &csi.NodeUnstageVolumeResponse{}, missingAttr("NodeUnstageVolume", req.VolumeId, "StagingTargetPath")
	}

	err := d.Mounter.Unmount(req.StagingTargetPath)
	if err != nil {
		return nil, err
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#nodepublishvolume
func (d Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return &csi.NodePublishVolumeResponse{}, missingAttr("NodePublishVolume", req.VolumeId, "VolumeId")
	}

	if req.StagingTargetPath == "" {
		return &csi.NodePublishVolumeResponse{}, missingAttr("NodePublishVolume", req.VolumeId, "StagingTargetPath")
	}

	if req.TargetPath == "" {
		return &csi.NodePublishVolumeResponse{}, missingAttr("NodePublishVolume", req.VolumeId, "TargetPath")
	}

	if req.VolumeCapability == nil {
		return &csi.NodePublishVolumeResponse{}, missingAttr("NodePublishVolume", req.VolumeId, "VolumeCapability slice")
	}

	mnt := req.VolumeCapability.GetMount()
	mntOpts := mnt.MountFlags
	mntOpts = append(mntOpts, "bind")
	if req.Readonly {
		mntOpts = append(mntOpts, "ro")
	}
	fsType := "ext4"
	if mnt.FsType != "" {
		fsType = mnt.FsType
	}

	existingVolume, err := d.Storage.GetByID(ctx, req.GetVolumeId())
	if err != nil {
		return nil, err
	}

	err = d.Mounter.Mount(existingVolume, req.StagingTargetPath, req.TargetPath, fsType, mntOpts)
	if err != nil {
		return nil, err
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#nodeunpublishvolume
func (d Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, missingAttr("NodeUnpublishVolume", req.VolumeId, "VolumeId")
	}

	if req.TargetPath == "" {
		return nil, missingAttr("NodeUnpublishVolume", req.VolumeId, "TargetPath")
	}

	err := d.Mounter.Unmount(req.TargetPath)
	if err != nil {
		return nil, err
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#nodegetvolumestats
func (d Driver) NodeGetVolumeStats(context.Context, *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return &csi.NodeGetVolumeStatsResponse{}, nil
}

// NodeGetCapabilities https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#nodegetcapabilities
func (d Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
				}}},
		},
	}, nil
}

// NodeGetInfo https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#nodegetinfo
func (d Driver) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId:            d.nodeID,
		MaxVolumesPerNode: 1048576, // DRBD volumes per node limit.
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				client.LinstorNodeTopologyKey: d.nodeID,
			}},
	}, nil
}

// CreateVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#createvolume
func (d Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return &csi.CreateVolumeResponse{}, missingAttr("CreateVolume", req.Name, "Name")
	}
	if req.VolumeCapabilities == nil || len(req.VolumeCapabilities) == 0 {
		return &csi.CreateVolumeResponse{}, missingAttr("ValidateVolumeCapabilities", req.Name, "VolumeCapabilities")
	}

	requiredKiB, err := d.Storage.AllocationSizeKiB(req.CapacityRange.GetRequiredBytes(), req.CapacityRange.GetLimitBytes())
	if err != nil {
		return &csi.CreateVolumeResponse{}, status.Error(
			codes.Internal, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
	}

	volumeSize := data.NewKibiByte(data.KiB * data.ByteSize(requiredKiB))

	// Handle case were a volume of the same name is already present.
	existingVolume, err := d.Storage.GetByName(ctx, req.Name)
	if err != nil {
		return &csi.CreateVolumeResponse{}, status.Error(
			codes.Internal, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
	}

	if existingVolume != nil {

		d.log.WithFields(logrus.Fields{
			"requestedVolumeName": req.Name,
			"existingVolume":      fmt.Sprintf("%+v", existingVolume),
		}).Info("volume already present")

		if existingVolume.CreatedBy != d.name {
			return &csi.CreateVolumeResponse{}, status.Error(
				codes.AlreadyExists, fmt.Sprintf(
					"CreateVolume failed for %s: volume already present and wasn't created by %s",
					req.Name, d.name))
		}

		if existingVolume.SizeBytes != int64(volumeSize.InclusiveBytes()) {
			return &csi.CreateVolumeResponse{}, status.Error(
				codes.AlreadyExists, fmt.Sprintf(
					"CreateVolume failed for %s: volume already present, created by %s, but size differs (existing: %d, wanted: %d",
					req.Name, d.name, existingVolume.SizeBytes, int64(volumeSize.InclusiveBytes())))
		}

		d.log.WithFields(logrus.Fields{
			"requestedVolumeName": req.Name,
			"volumeSize":          volumeSize,
			"expectedCreatedBy":   d.name,
			"existingVolume":      fmt.Sprintf("%+v", existingVolume),
		}).Info("volume already present, but matches request")

		topos, err := d.Storage.AccessibleTopologies(ctx, existingVolume)
		if err != nil {
			return &csi.CreateVolumeResponse{}, status.Errorf(
				codes.Internal, "CreateVolume failed for %s: unable to determine volume topology: %v",
				req.Name, err)
		}

		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:           existingVolume.ID,
				CapacityBytes:      existingVolume.SizeBytes,
				AccessibleTopology: topos,
			}}, nil
	}

	return d.createNewVolume(ctx, req)
}

// DeleteVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#deletevolume
func (d Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, missingAttr("DeleteVolume", req.VolumeId, "VolumeId")
	}

	existingVolume, err := d.Storage.GetByID(ctx, req.GetVolumeId())
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

	if err := d.Storage.Delete(ctx, existingVolume); err != nil {
		return nil, err
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#controllerpublishvolume
func (d Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, missingAttr("ControllerPublishVolume", req.VolumeId, "VolumeId")
	}
	if req.NodeId == "" {
		return nil, missingAttr("ControllerPublishVolume", req.VolumeId, "NodeId")
	}
	if req.VolumeCapability == nil {
		return nil, missingAttr("ControllerPublishVolume", req.VolumeId, "VolumeCapability")
	}

	// Don't try to assign volumes that don't exist.
	existingVolume, err := d.Storage.GetByID(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Error(
			codes.Internal, fmt.Sprintf(
				"ControllerPublishVolume failed for %s: %v", req.VolumeId, err))
	}
	if existingVolume == nil {
		return nil, status.Error(
			codes.NotFound, fmt.Sprintf(
				"ControllerPublishVolume failed for %s: volume not present in storage backend",
				req.VolumeId))
	}
	d.log.WithFields(logrus.Fields{
		"existingVolume": fmt.Sprintf("%+v", existingVolume),
	}).Debug("found existing volume")

	// Or have had their attributes changed.
	if existingVolume.Readonly != req.Readonly {
		return nil, status.Error(
			codes.AlreadyExists, fmt.Sprintf(
				"ControllerPublishVolume failed for %s: volume attributes changes for already existing volume",
				req.VolumeId))
	}

	// Don't even attempt to put it on nodes that aren't avaible.
	if err := d.Assignments.NodeAvailable(ctx, req.NodeId); err != nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf(
			"ControllerPublishVolume failed for %s on node %s: %v", req.VolumeId, req.GetNodeId(), err))
	}

	err = d.Assignments.Attach(ctx, existingVolume, req.NodeId)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf(
			"ControllerPublishVolume failed for %s: %v", req.VolumeId, err))
	}

	return &csi.ControllerPublishVolumeResponse{}, nil
}

// ControllerUnpublishVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#controllerunpublishvolume
func (d Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, missingAttr("ControllerUnpublishVolume", req.GetVolumeId(), "VolumeId")
	}
	if req.GetNodeId() == "" {
		return nil, missingAttr("ControllerUnpublishVolume", req.GetNodeId(), "NodeId")
	}

	vol, err := d.Storage.GetByID(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Error(
			codes.Internal, fmt.Sprintf(
				"ControllerUnpublishVolume failed for %s: %v", req.GetVolumeId(), err))
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
		return nil, status.Error(codes.Internal, fmt.Sprintf(
			"ControllerpublishVolume failed for %s: %v", req.GetVolumeId(), err))
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#validatevolumecapabilities
func (d Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, missingAttr("ValidateVolumeCapabilities", req.VolumeId, "volumeId")
	}

	if req.VolumeCapabilities == nil {
		return nil, missingAttr("ValidateVolumeCapabilities", req.VolumeId, "VolumeCapabilities")
	}

	existingVolume, err := d.Storage.GetByID(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Error(
			codes.Internal, fmt.Sprintf("ValidateVolumeCapabilities failed for %s: %v", req.VolumeId, err))
	}
	if existingVolume == nil {
		return nil, status.Error(
			codes.NotFound, fmt.Sprintf("ValidateVolumeCapabilities failed for %s: volume not present in storage backend", req.VolumeId))
	}
	d.log.WithFields(logrus.Fields{
		"existingVolume": fmt.Sprintf("%+v", existingVolume),
	}).Debug("found existing volume")

	supportedCapabilities := []*csi.VolumeCapability{
		{AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
	}

	for _, reqCap := range req.VolumeCapabilities {
		if reqCap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: supportedCapabilities,
		}}, nil
}

// ListVolumes https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#getcapacity
func (d Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {

	start, err := parseStartingToken(req.GetStartingToken())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ListVolumes failed for: %v", err)
	}

	volumes, err := d.Storage.ListAll(ctx, int(req.GetMaxEntries()), start)
	if err != nil {
		return &csi.ListVolumesResponse{}, status.Errorf(codes.Internal, "ListVolumes failed: %v", err)
	}

	var entries = make([]*csi.ListVolumesResponse_Entry, len(volumes))
	for _, vol := range volumes {
		topos, err := d.Storage.AccessibleTopologies(ctx, vol)
		if err != nil {
			return nil, status.Errorf(
				codes.Internal, "ListVolumes failed for %s: unable to determine volume topology: %v",
				vol.Name, err)
		}

		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:           vol.ID,
				CapacityBytes:      vol.SizeBytes,
				AccessibleTopology: topos,
			}})
	}

	return &csi.ListVolumesResponse{NextToken: strconv.Itoa(int(start + 1)), Entries: entries}, nil
}

// GetCapacity https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#getcapacity
func (d Driver) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	bytes, err := d.Storage.CapacityBytes(ctx, req.GetParameters())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &csi.GetCapacityResponse{AvailableCapacity: bytes}, nil
}

// ControllerGetCapabilities https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#controllergetcapabilities
func (d Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
				}}},
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
				}}},
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
				}}},
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
				}}},
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
				}}},
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
				}}},
			{Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: csi.ControllerServiceCapability_RPC_GET_CAPACITY,
				}}},
		},
	}, nil
}

// CreateSnapshot https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#createsnapshot
func (d Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if req.GetSourceVolumeId() == "" {
		return nil, missingAttr("CreateSnapshot", req.SourceVolumeId, "SourceVolumeId")
	}
	if req.GetName() == "" {
		return nil, missingAttr("CreateSnapshot", req.Name, "Name")
	}

	existingSnap, err := d.Snapshots.GetSnapByName(ctx, req.GetName())
	if err != nil {
		return nil, fmt.Errorf("failed to check for existing snapshot: %v", err)
	}

	if existingSnap != nil {
		// Needed for idempotentcy.
		d.log.WithFields(logrus.Fields{
			"requestedSnapshotName":         req.GetName(),
			"requestedSnapshotSourceVolume": req.GetSourceVolumeId(),
			"existingSnapshot":              fmt.Sprintf("%+v", existingSnap),
		}).Debug("found existing snapshot")
		if existingSnap.CsiSnap.SourceVolumeId == req.GetSourceVolumeId() {
			return &csi.CreateSnapshotResponse{Snapshot: existingSnap.CsiSnap}, nil
		}
		return nil, status.Errorf(codes.AlreadyExists, "can't use %q for snapshot name for volume %q, snapshot name is in use for volume %q",
			existingSnap.Name, req.GetSourceVolumeId(), existingSnap.CsiSnap.SourceVolumeId)
	}

	snap, err := d.Snapshots.SnapCreate(ctx, &volume.SnapInfo{
		Name:    d.Snapshots.CanonicalizeSnapshotName(ctx, req.GetName()),
		CsiSnap: &csi.Snapshot{SourceVolumeId: req.GetSourceVolumeId()},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot: %v", err)
	}

	return &csi.CreateSnapshotResponse{Snapshot: snap.CsiSnap}, nil
}

// DeleteSnapshot https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#deletesnapshot
func (d Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, missingAttr("DeleteSnapshot", req.SnapshotId, "SnapshotId")
	}

	snap, err := d.Snapshots.GetSnapByID(ctx, req.GetSnapshotId())
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

// ListSnapshots https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#listsnapshots
func (d Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	snapshots := make([]*volume.SnapInfo, 0)

	// Handle case where a single snapshot is requsted.
	if req.GetSnapshotId() != "" {
		snap, err := d.Snapshots.GetSnapByID(ctx, req.GetSnapshotId())
		if err != nil {
			return nil, fmt.Errorf("failed to list snapshots: %v", err)
		}
		if snap == nil {
			return &csi.ListSnapshotsResponse{}, nil
		}
		d.log.WithFields(logrus.Fields{
			"requestedSnapshot": req.GetSnapshotId(),
			"napshot":           fmt.Sprintf("%+v", snap),
		}).Debug("found single snapshot")
		snapshots = []*volume.SnapInfo{snap}

		// Handle case where a single volumes snapshots are requested.
	} else if req.GetSourceVolumeId() != "" {
		vol, err := d.Storage.GetByID(ctx, req.GetSourceVolumeId())
		if err != nil {
			return nil, fmt.Errorf("failed to list snapshots: %v", err)
		}
		if vol == nil {
			return &csi.ListSnapshotsResponse{}, nil
		}

		snapshots = vol.Snapshots

		// Regular list of all snapshots.
	} else {
		snaps, err := d.Snapshots.ListSnaps(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list snapshots: %v", err)
		}
		snapshots = snaps
	}

	// Handle pagination.
	var (
		end       int32
		nextToken string
	)
	totalSnaps := int32(len(snapshots))
	if req.GetMaxEntries() == 0 || (totalSnaps <= req.GetMaxEntries()) {
		end = totalSnaps
	} else {
		end = req.GetMaxEntries()
		nextToken = strconv.Itoa(int(end))
	}

	start, err := parseStartingToken(req.GetStartingToken())
	if err != nil {
		return nil, fmt.Errorf("failed to list snapshots: %v", err)
	}

	snapshots = snapshots[start:end]

	entries := make([]*csi.ListSnapshotsResponse_Entry, 0)

	for _, snap := range snapshots {
		entries = append(entries, &csi.ListSnapshotsResponse_Entry{Snapshot: snap.CsiSnap})
	}
	return &csi.ListSnapshotsResponse{Entries: entries, NextToken: nextToken}, nil
}

// NodeExpandVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#nodeexpandvolume
func (d Driver) NodeExpandVolume(context.Context, *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	panic("not implemented")
}

// ControllerExpandVolume https://github.com/container-storage-interface/spec/blob/v1.1.0/spec.md#controllerexpandvolume
func (d Driver) ControllerExpandVolume(context.Context, *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	panic("not implemented")
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
	return fmt.Errorf("Not implemented")
}

func (d Driver) createNewVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	requiredKiB, err := d.Storage.AllocationSizeKiB(req.CapacityRange.GetRequiredBytes(), req.CapacityRange.GetLimitBytes())
	if err != nil {
		return &csi.CreateVolumeResponse{}, status.Error(
			codes.Internal, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
	}

	volumeSize := data.NewKibiByte(data.KiB * data.ByteSize(requiredKiB))

	vol := &volume.Info{
		Name:         req.Name,
		SizeBytes:    int64(volumeSize.InclusiveBytes()),
		CreatedBy:    d.name,
		CreationTime: time.Now(),
		Parameters:   make(map[string]string),
		Snapshots:    []*volume.SnapInfo{},
	}
	for k, v := range req.Parameters {
		vol.Parameters[k] = v
	}
	d.log.WithFields(logrus.Fields{
		"newVolume": fmt.Sprintf("%+v", vol),
		"size":      volumeSize,
	}).Debug("creating new volume")

	// We're cloning from a volume or snapshot.
	if req.GetVolumeContentSource() != nil {
		if req.GetVolumeContentSource().GetSnapshot() != nil {
			snapshotId := req.GetVolumeContentSource().GetSnapshot().GetSnapshotId()
			if snapshotId == "" {
				return &csi.CreateVolumeResponse{}, status.Error(codes.InvalidArgument, fmt.Sprintf("CreateVolume failed for %s: empty snapshotId", req.Name))
			}

			snap, err := d.Snapshots.GetSnapByID(ctx, snapshotId)
			if err != nil {
				return &csi.CreateVolumeResponse{}, status.Error(
					codes.Internal, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
			}
			if snap == nil {
				return &csi.CreateVolumeResponse{}, status.Error(
					codes.NotFound, fmt.Sprintf("CreateVolume failed for %s: snapshot not found in storage backend", req.Name))
			}
			sourceVol, err := d.Storage.GetByID(ctx, snap.CsiSnap.SourceVolumeId)
			if err != nil {
				return &csi.CreateVolumeResponse{}, status.Error(
					codes.Internal, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
			}
			if sourceVol == nil {
				return &csi.CreateVolumeResponse{}, status.Error(
					codes.NotFound, fmt.Sprintf("CreateVolume failed for %s: source volume not found in storage backend", req.Name))
			}

			if err := d.Snapshots.VolFromSnap(ctx, snap, vol); err != nil {
				d.Storage.Delete(ctx, vol)
				return &csi.CreateVolumeResponse{}, status.Error(
					codes.Internal, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
			}
			// We're cloning from a whole volume.
		} else if req.GetVolumeContentSource().GetVolume() != nil {
			sourceVol, err := d.Storage.GetByID(ctx, req.GetVolumeContentSource().GetVolume().GetVolumeId())
			if err != nil {
				return &csi.CreateVolumeResponse{}, status.Error(
					codes.Internal, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
			}
			if sourceVol == nil {
				return &csi.CreateVolumeResponse{}, status.Error(
					codes.NotFound, fmt.Sprintf("CreateVolume failed for %s: source volume not found in storage backend", req.Name))
			}
			if err := d.Snapshots.VolFromVol(ctx, sourceVol, vol); err != nil {
				d.Storage.Delete(ctx, vol)
				return &csi.CreateVolumeResponse{}, status.Error(
					codes.Internal, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
			}
		} else {
			return &csi.CreateVolumeResponse{}, status.Error(
				codes.InvalidArgument, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
		}
		// Regular new volume.
	} else {
		if err := d.Storage.Create(ctx, vol, req); err != nil {
			d.Storage.Delete(ctx, vol)
			return &csi.CreateVolumeResponse{}, status.Error(
				codes.Internal, fmt.Sprintf("CreateVolume failed for %s: %v", req.Name, err))
		}
	}

	topos, err := d.Storage.AccessibleTopologies(ctx, vol)
	if err != nil {
		return &csi.CreateVolumeResponse{}, status.Errorf(
			codes.Internal, "CreateVolume failed for %s: unable to determine volume topology: %v",
			req.Name, err)
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           vol.ID,
			CapacityBytes:      int64(volumeSize.InclusiveBytes()),
			AccessibleTopology: topos,
		}}, nil
}

func missingAttr(methodCall, volumeID, attr string) error {
	if volumeID == "" {
		volumeID = "an unknown volume"
	}
	return status.Error(codes.InvalidArgument, fmt.Sprintf("%s failed for %s: it requires a %s and none was provided", methodCall, volumeID, attr))
}

func parseStartingToken(s string) (int, error) {
	if s == "" {
		return int(0), nil
	}

	i, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to parse starting token: %v", err)
	}
	return int(i), nil
}
