// Copyright 2022 NetApp, Inc. All Rights Reserved.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"

	. "github.com/netapp/trident/logging"
	sa "github.com/netapp/trident/storage_attribute"
	"github.com/netapp/trident/storage_drivers/ontap/api/rest/client/n_a_s"
	"github.com/netapp/trident/storage_drivers/ontap/api/rest/models"
	"github.com/netapp/trident/utils"
	versionutils "github.com/netapp/trident/utils/version"
)

// RestError encapsulates the status, reason, and errno values from a REST invocation, and it provides helper methods for detecting
// common error conditions.
type RestError struct {
	// 	"uuid": "2453aafc-a9a6-11eb-9fc7-080027c8f2a7",
	// 	"description": "DELETE /api/storage/volumes/60018ffd-a9a3-11eb-9fc7-080027c8f2a7/snapshots/6365e696-a9a3-11eb-9fc7-080027c8f2a7",
	// 	"state": "failure",
	// 	"message": "Snapshot copy \"snapshot-60f627c7-576b-42a5-863e-9ea174856f2f\" of volume \"rippy_pvc_e8f1cc49_7949_403c_9f83_786d1480af38\" on Vserver \"nfs_vs\" has not expired or is locked. Use the \"snapshot show -fields owners, expiry-time\" command to view the expiry and lock status of the Snapshot copy.",
	// 	"code": 1638555,
	// 	"start_time": "2021-04-30T07:21:00-04:00",
	// 	"end_time": "2021-04-30T07:21:10-04:00",
	uuid        string
	description string
	state       string
	message     string
	code        string
	start_time  string
	end_time    string
}

func NewRestErrorFromPayload(payload *models.Job) RestError {
	restError := RestError{}
	if payload == nil {
		return restError
	}

	if payload.UUID != nil {
		restError.uuid = payload.UUID.String()
	}

	if payload.Description != nil {
		restError.description = *payload.Description
	}

	if payload.State != nil {
		restError.state = *payload.State
	}

	if payload.Message != nil {
		restError.message = *payload.Message
	}

	if payload.Code != nil {
		restError.code = fmt.Sprint(*payload.Code)
	}

	if payload.StartTime != nil {
		restError.start_time = payload.StartTime.String()
	}

	if payload.EndTime != nil {
		restError.end_time = payload.EndTime.String()
	}

	return restError
}

func (e RestError) IsSuccess() bool {
	return e.state == models.JobStateSuccess
}

func (e RestError) IsFailure() bool {
	return e.state == models.JobStateFailure
}

func (e RestError) Error() string {
	if e.IsSuccess() {
		return "API status: success"
	}
	return fmt.Sprintf("API State: %s, Message: %s, Code: %s", e.state, e.message, e.code)
}

func (e RestError) IsSnapshotBusy() bool {
	return e.code == ESNAPSHOTBUSY_REST
}

func (e RestError) State() string {
	return e.state
}

func (e RestError) Message() string {
	return e.message
}

func (e RestError) Code() string {
	return e.code
}

type OntapAPIREST struct {
	api        RestClientInterface
	driverName string
}

func NewOntapAPIREST(restClient *RestClient, driverName string) (OntapAPIREST, error) {
	result := OntapAPIREST{
		api:        restClient,
		driverName: driverName,
	}

	return result, nil
}

// NewOntapAPIRESTFromRestClientInterface added for testing
func NewOntapAPIRESTFromRestClientInterface(restClient RestClientInterface) (OntapAPIREST, error) {
	result := OntapAPIREST{
		api:        restClient,
		driverName: "",
	}

	return result, nil
}

func (d OntapAPIREST) SVMName() string {
	return d.api.SVMName()
}

func (d OntapAPIREST) ValidateAPIVersion(ctx context.Context) error {
	// Make sure we're using a valid ONTAP version
	ontapVersion, err := d.APIVersion(ctx)
	if err != nil {
		return fmt.Errorf("could not determine Data ONTAP version: %v", err)
	}
	Logc(ctx).WithField("ontapVersion", ontapVersion).Debug("ONTAP version.")

	ontapSemVer, err := versionutils.ParseSemantic(ontapVersion)
	if err != nil {
		return err
	}
	if !ontapSemVer.AtLeast(MinimumONTAPVersion) {
		return fmt.Errorf("ONTAP %s or later is required, found %v", MinimumONTAPVersion.String(), ontapVersion)
	}
	return nil
}

func (d OntapAPIREST) VolumeCreate(ctx context.Context, volume Volume) error {
	fields := LogFields{
		"Method": "VolumeCreate",
		"Type":   "OntapAPIREST",
		"spec":   volume,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> VolumeCreate")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< VolumeCreate")

	creationErr := d.api.VolumeCreate(ctx, volume.Name, volume.Aggregates[0], volume.Size, volume.SpaceReserve,
		volume.SnapshotPolicy, volume.UnixPermissions, volume.ExportPolicy, volume.SecurityStyle,
		volume.TieringPolicy, volume.Comment, volume.Qos, volume.Encrypt, volume.SnapshotReserve, volume.DPVolume)
	if creationErr != nil {
		return fmt.Errorf("error creating volume: %v", creationErr)
	}

	return nil
}

func (d OntapAPIREST) VolumeDestroy(ctx context.Context, name string, force bool) error {
	deletionErr := d.api.VolumeDestroy(ctx, name)
	if deletionErr != nil {
		return fmt.Errorf("error destroying volume %v: %v", name, deletionErr)
	}
	return nil
}

func (d OntapAPIREST) VolumeInfo(ctx context.Context, name string) (*Volume, error) {
	volumeGetResponse, err := d.api.VolumeGetByName(ctx, name)
	if err != nil {
		Logc(ctx).Errorf("Could not find volume with name: %v, error: %v", name, err.Error())
		return nil, err
	}

	if volumeGetResponse == nil {
		Logc(ctx).Errorf("Could not find volume with name: %v", name)
		return nil, VolumeReadError(fmt.Sprintf("could not find volume with name %s", name))
	}

	volumeInfo, err := VolumeInfoFromRestAttrsHelper(volumeGetResponse)
	if err != nil {
		return nil, err
	}

	return volumeInfo, nil
}

func VolumeInfoFromRestAttrsHelper(volumeGetResponse *models.Volume) (*Volume, error) {
	var responseAccessType string
	var responseAggregates []string
	var responseComment string
	var responseExportPolicy string
	var responseJunctionPath string
	var responseSize string
	// var responseSnapdirAccessEnabled bool
	var responseSnapshotPolicy string
	var responseSnapshotReserveInt int
	var responseSnapshotSpaceUsed int
	var responseSpaceReserve string
	var responseUnixPermissions string

	if volumeGetResponse == nil {
		return nil, fmt.Errorf("volumeGetResponse was nil")
	}
	if volumeGetResponse.Name == nil {
		return nil, fmt.Errorf("volumeGetResponse name was nil")
	}

	if volumeGetResponse.Type != nil {
		responseAccessType = *volumeGetResponse.Type
	}

	if len(volumeGetResponse.VolumeInlineAggregates) > 0 {
		if volumeGetResponse.VolumeInlineAggregates[0].Name != nil {
			responseAggregates = []string{
				*volumeGetResponse.VolumeInlineAggregates[0].Name,
			}
		}
	} else {
		return nil, VolumeIdAttributesReadError(fmt.Sprintf("error reading aggregates for volume %s",
			*volumeGetResponse.Name))
	}

	if volumeGetResponse.Comment != nil {
		responseComment = *volumeGetResponse.Comment
	}

	if volumeGetResponse.Nas != nil {
		if volumeGetResponse.Nas.Path != nil {
			responseJunctionPath = *volumeGetResponse.Nas.Path
		}

		if volumeGetResponse.Nas.ExportPolicy != nil {
			if volumeGetResponse.Nas.ExportPolicy.Name != nil {
				responseExportPolicy = *volumeGetResponse.Nas.ExportPolicy.Name
			}
		}

		if volumeGetResponse.Nas.UnixPermissions != nil {
			responseUnixPermissions = strconv.FormatInt(*volumeGetResponse.Nas.UnixPermissions, 8)
		}
	}

	if volumeGetResponse.Size != nil {
		responseSize = strconv.FormatInt(*volumeGetResponse.Size, 10)
	}

	if volumeGetResponse.Guarantee != nil && volumeGetResponse.Guarantee.Type != nil {
		responseSpaceReserve = *volumeGetResponse.Guarantee.Type
	}

	if volumeGetResponse.SnapshotPolicy != nil && volumeGetResponse.SnapshotPolicy.Name != nil {
		responseSnapshotPolicy = *volumeGetResponse.SnapshotPolicy.Name
	}

	if volumeGetResponse.Space != nil {
		if volumeGetResponse.Space.Snapshot != nil {
			if volumeGetResponse.Space.Snapshot.ReservePercent != nil {
				responseSnapshotReserveInt = int(*volumeGetResponse.Space.Snapshot.ReservePercent)
			}
			if volumeGetResponse.Space.Snapshot.Used != nil {
				responseSnapshotSpaceUsed = int(*volumeGetResponse.Space.Snapshot.Used)
			}
		}
	}

	snapshotDir := false
	if volumeGetResponse.SnapshotDirectoryAccessEnabled != nil {
		snapshotDir = *volumeGetResponse.SnapshotDirectoryAccessEnabled
	}

	volumeInfo := &Volume{
		AccessType:        responseAccessType,
		Aggregates:        responseAggregates,
		Comment:           responseComment,
		ExportPolicy:      responseExportPolicy,
		JunctionPath:      responseJunctionPath,
		Size:              responseSize,
		SnapshotDir:       snapshotDir,
		SnapshotPolicy:    responseSnapshotPolicy,
		SnapshotReserve:   responseSnapshotReserveInt,
		SnapshotSpaceUsed: responseSnapshotSpaceUsed,
		SpaceReserve:      responseSpaceReserve,
		UnixPermissions:   responseUnixPermissions,
		DPVolume:          responseAccessType == "dp",
	}

	if volumeGetResponse.Name != nil {
		volumeInfo.Name = *volumeGetResponse.Name
	}

	if volumeGetResponse.UUID != nil {
		volumeInfo.UUID = *volumeGetResponse.UUID
	}

	return volumeInfo, nil
}

func lunInfoFromRestAttrsHelper(lunGetResponse *models.Lun) (*Lun, error) {
	var responseComment string
	var responseLunMaps []LunMap
	var responseQos string
	var responseSize string
	var responseMapped bool
	var responseVolName string
	var responseCreateTime string

	if lunGetResponse == nil {
		return nil, fmt.Errorf("lun response is nil")
	}
	if lunGetResponse.Comment != nil {
		responseComment = *lunGetResponse.Comment
	}

	var lunMap LunMap
	for _, record := range lunGetResponse.LunInlineLunMaps {
		if record.Igroup != nil && record.Igroup.Name != nil {
			lunMap.IgroupName = *record.Igroup.Name
		}
		if record.LogicalUnitNumber != nil {
			lunMap.LunID = int(*record.LogicalUnitNumber)
		}
		responseLunMaps = append(responseLunMaps, lunMap)
	}

	if lunGetResponse.Space != nil && lunGetResponse.Space.Size != nil {
		responseSize = strconv.FormatInt(*lunGetResponse.Space.Size, 10)
	}

	if lunGetResponse.Comment != nil {
		responseComment = *lunGetResponse.Comment
	}

	if lunGetResponse.QosPolicy != nil && lunGetResponse.QosPolicy.Name != nil {
		responseQos = *lunGetResponse.QosPolicy.Name
	}

	if lunGetResponse.Status.Mapped != nil {
		responseMapped = *lunGetResponse.Status.Mapped
	}

	if lunGetResponse.Location != nil {
		if lunGetResponse.Location.Volume != nil && lunGetResponse.Location.Volume.Name != nil {
			responseVolName = *lunGetResponse.Location.Volume.Name
		}
	}

	if lunGetResponse.CreateTime != nil {
		responseCreateTime = lunGetResponse.CreateTime.String()
	}

	enabled := false
	if lunGetResponse.Enabled != nil {
		enabled = *lunGetResponse.Enabled
	}

	name := ""
	if lunGetResponse.Name != nil {
		name = *lunGetResponse.Name
	}

	uuid := ""
	if lunGetResponse.UUID != nil {
		uuid = *lunGetResponse.UUID
	}

	serialNumber := ""
	if lunGetResponse.SerialNumber != nil {
		serialNumber = *lunGetResponse.SerialNumber
	}

	state := ""
	if lunGetResponse.Status != nil && lunGetResponse.Status.State != nil {
		state = *lunGetResponse.Status.State
	}

	lunInfo := &Lun{
		Comment:      responseComment,
		CreateTime:   responseCreateTime,
		Enabled:      enabled,
		LunMaps:      responseLunMaps,
		Name:         name,
		Qos:          QosPolicyGroup{Name: responseQos},
		Size:         responseSize,
		Mapped:       responseMapped,
		UUID:         uuid,
		SerialNumber: serialNumber,
		State:        state,
		VolumeName:   responseVolName,
	}
	return lunInfo, nil
}

func (d OntapAPIREST) APIVersion(ctx context.Context) (string, error) {
	return d.api.SystemGetOntapVersion(ctx)
}

func (d OntapAPIREST) NodeListSerialNumbers(ctx context.Context) ([]string, error) {
	return d.api.NodeListSerialNumbers(ctx)
}

func (d OntapAPIREST) SupportsFeature(ctx context.Context, feature Feature) bool {
	return d.api.SupportsFeature(ctx, feature)
}

func (d OntapAPIREST) NetInterfaceGetDataLIFs(ctx context.Context, protocol string) ([]string, error) {
	return d.api.NetInterfaceGetDataLIFs(ctx, protocol)
}

func (d OntapAPIREST) GetSVMAggregateNames(ctx context.Context) ([]string, error) {
	return d.api.SVMGetAggregateNames(ctx)
}

func (d OntapAPIREST) EmsAutosupportLog(
	ctx context.Context,
	driverName string,
	appVersion string,
	autoSupport bool,
	category string,
	computerName string,
	eventDescription string,
	eventID int,
	eventSource string,
	logLevel int,
) {
	if err := d.api.EmsAutosupportLog(ctx, appVersion, autoSupport, category, computerName, eventDescription, eventID,
		eventSource, logLevel); err != nil {
		Logc(ctx).WithError(err).Warn("Could not generate Autosupport message.")
	}
}

func (d OntapAPIREST) FlexgroupExists(ctx context.Context, volumeName string) (bool, error) {
	return d.api.FlexGroupExists(ctx, volumeName)
}

func (d OntapAPIREST) FlexgroupCreate(ctx context.Context, volume Volume) error {
	fields := LogFields{
		"Method": "FlexgroupCreate",
		"Type":   "OntapAPIREST",
		"spec":   volume,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> FlexgroupCreate")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< FlexgroupCreate")

	volumeSize, _ := strconv.ParseUint(volume.Size, 10, 64)

	creationErr := d.api.FlexGroupCreate(ctx, volume.Name, int(volumeSize), volume.Aggregates, volume.SpaceReserve,
		volume.SnapshotPolicy, volume.UnixPermissions, volume.ExportPolicy, volume.SecurityStyle, volume.TieringPolicy,
		volume.Comment, volume.Qos, volume.Encrypt, volume.SnapshotReserve)
	if creationErr != nil {
		return fmt.Errorf("error creating volume: %v", creationErr)
	}

	return nil
}

func (d OntapAPIREST) FlexgroupCloneSplitStart(ctx context.Context, cloneName string) error {
	if err := d.api.FlexgroupCloneSplitStart(ctx, cloneName); err != nil {
		return fmt.Errorf("error splitting clone: %v", err)
	}
	return nil
}

func (d OntapAPIREST) FlexgroupDisableSnapshotDirectoryAccess(ctx context.Context, volumeName string) error {
	if err := d.api.FlexGroupVolumeDisableSnapshotDirectoryAccess(ctx, volumeName); err != nil {
		return fmt.Errorf("error disabling snapshot directory access: %v", err)
	}

	return nil
}

func (d OntapAPIREST) FlexgroupInfo(ctx context.Context, volumeName string) (*Volume, error) {
	volumeGetResponse, err := d.api.FlexGroupGetByName(ctx, volumeName)
	if err != nil {
		Logc(ctx).Errorf("Could not find volume with name: %v, error: %v", volumeName, err.Error())
		return nil, err
	}

	if volumeGetResponse == nil {
		Logc(ctx).Errorf("Could not find volume with name: %v", volumeName)
		return nil, VolumeReadError(fmt.Sprintf("could not find volume with name %s", volumeName))
	}

	volumeInfo, err := VolumeInfoFromRestAttrsHelper(volumeGetResponse)
	if err != nil {
		return nil, err
	}

	return volumeInfo, nil
}

func (d OntapAPIREST) FlexgroupSetComment(
	ctx context.Context, volumeNameInternal, volumeNameExternal, comment string,
) error {
	if err := d.api.FlexGroupSetComment(ctx, volumeNameInternal, comment); err != nil {
		Logc(ctx).WithField("originalName", volumeNameExternal).Errorf("Modifying comment failed: %v", err)
		return fmt.Errorf("volume %s modify failed: %v", volumeNameExternal, err)
	}
	return nil
}

func (d OntapAPIREST) FlexgroupSetQosPolicyGroupName(ctx context.Context, name string, qos QosPolicyGroup) error {
	if err := d.api.FlexgroupSetQosPolicyGroupName(ctx, name, qos); err != nil {
		return fmt.Errorf("error setting quality of service policy: %v", err)
	}

	return nil
}

func (d OntapAPIREST) FlexgroupSnapshotCreate(ctx context.Context, snapshotName, sourceVolume string) error {
	volume, err := d.FlexgroupInfo(ctx, sourceVolume)
	if err != nil {
		return fmt.Errorf("error looking up source volume %v: %v", sourceVolume, err)
	}
	if volume == nil {
		return fmt.Errorf("error looking up source volume: %v", sourceVolume)
	}

	if err = d.api.SnapshotCreateAndWait(ctx, volume.UUID, snapshotName); err != nil {
		return fmt.Errorf("could not create snapshot: %v", err)
	}
	return nil
}

func (d OntapAPIREST) FlexgroupSnapshotList(ctx context.Context, sourceVolume string) (Snapshots, error) {
	volume, err := d.FlexgroupInfo(ctx, sourceVolume)
	if err != nil {
		return nil, fmt.Errorf("error looking up source volume: %v", err)
	}
	if volume == nil {
		return nil, fmt.Errorf("error looking up source volume: %v", sourceVolume)
	}

	snapListResponse, err := d.api.SnapshotList(ctx, volume.UUID)
	if err != nil {
		return nil, fmt.Errorf("error enumerating snapshots: %v", err)
	}
	if snapListResponse == nil {
		return nil, fmt.Errorf("error enumerating snapshots")
	}
	if snapListResponse.Payload == nil {
		return nil, fmt.Errorf("error enumerating snapshots")
	}

	snapshots := Snapshots{}
	for _, snap := range snapListResponse.Payload.SnapshotResponseInlineRecords {
		if snap.CreateTime != nil && snap.Name != nil {
			snapshots = append(snapshots, Snapshot{
				CreateTime: snap.CreateTime.String(),
				Name:       *snap.Name,
			})
		}
	}

	Logc(ctx).Debugf("Returned %v snapshots.", snapListResponse.Payload.NumRecords)

	return snapshots, nil
}

func (d OntapAPIREST) FlexgroupModifyUnixPermissions(
	ctx context.Context, volumeNameInternal, volumeNameExternal, unixPermissions string,
) error {
	err := d.api.FlexGroupModifyUnixPermissions(ctx, volumeNameInternal, unixPermissions)
	if err != nil {
		Logc(ctx).WithField("originalName", volumeNameExternal).Errorf("Could not import volume, "+
			"modifying unix permissions failed: %v", err)
		return fmt.Errorf("volume %s modify failed: %v", volumeNameExternal, err)
	}

	return nil
}

func (d OntapAPIREST) FlexgroupMount(ctx context.Context, name, junctionPath string) error {
	// Mount the volume at the specified junction
	if err := d.api.FlexGroupMount(ctx, name, junctionPath); err != nil {
		return fmt.Errorf("error mounting volume %v to junction %v: %v", name, junctionPath, err)
	}

	return nil
}

func (d OntapAPIREST) FlexgroupDestroy(ctx context.Context, volumeName string, force bool) error {
	if err := d.FlexgroupUnmount(ctx, volumeName, true); err != nil {
		return fmt.Errorf("error unmounting volume %v: %v", volumeName, err)
	}

	// TODO: If this is the parent of one or more clones, those clones have to split from this
	// volume before it can be deleted, which means separate copies of those volumes.
	// If there are a lot of clones on this volume, that could seriously balloon the amount of
	// utilized space. Is that what we want? Or should we just deny the delete, and force the
	// user to keep the volume around until all of the clones are gone? If we do that, need a
	// way to list the clones. Maybe volume inspect.
	deletionErr := d.api.FlexGroupDestroy(ctx, volumeName)
	if deletionErr != nil {
		return fmt.Errorf("error destroying volume %v: %v", volumeName, deletionErr)
	}
	return nil
}

func (d OntapAPIREST) FlexgroupListByPrefix(ctx context.Context, prefix string) (Volumes, error) {
	// TODO handle this higher? or just leave this here? i think here is OK
	if !strings.HasSuffix(prefix, "*") {
		// append the "*" to our prefix if it's missing
		prefix += "*"
	}

	flexgroupsResponse, err := d.api.FlexGroupGetAll(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if flexgroupsResponse == nil || flexgroupsResponse.Payload == nil {
		return nil, fmt.Errorf("flexgroupsResponse was nil")
	}

	volumes := Volumes{}
	for _, volume := range flexgroupsResponse.Payload.VolumeResponseInlineRecords {
		volumeInfo, err := VolumeInfoFromRestAttrsHelper(volume)
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, volumeInfo)
	}

	return volumes, nil
}

func (d OntapAPIREST) FlexgroupSetSize(ctx context.Context, name, newSize string) error {
	if err := d.api.FlexGroupSetSize(ctx, name, newSize); err != nil {
		Logc(ctx).WithField("error", err).Error("Volume resize failed.")
		return fmt.Errorf("volume resize failed")
	}

	return nil
}

func (d OntapAPIREST) FlexgroupSize(ctx context.Context, volumeName string) (uint64, error) {
	return d.api.FlexGroupSize(ctx, volumeName)
}

func (d OntapAPIREST) FlexgroupUsedSize(ctx context.Context, volumeName string) (int, error) {
	return d.api.FlexGroupUsedSize(ctx, volumeName)
}

func (d OntapAPIREST) FlexgroupModifyExportPolicy(ctx context.Context, volumeName, policyName string) error {
	err := d.api.FlexgroupModifyExportPolicy(ctx, volumeName, policyName)
	if err != nil {
		err = fmt.Errorf("error updating export policy on volume %s: %v", volumeName, err)
		Logc(ctx).Error(err)
		return err
	}

	return nil
}

func (d OntapAPIREST) FlexgroupUnmount(ctx context.Context, name string, _ bool) error {
	// Setting an empty path should deactivate and unmount the volume
	if err := d.api.FlexgroupUnmount(ctx, name); err != nil {
		return fmt.Errorf("error unmounting volume %v: %v", name, err)
	}

	return nil
}

func (d OntapAPIREST) GetSVMAggregateAttributes(ctx context.Context) (aggrList map[string]string, err error) {
	// Handle panics from the API layer
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("unable to inspect ONTAP backend: %v\nStack trace:\n%s", r, debug.Stack())
		}
	}()

	result, err := d.api.AggregateList(ctx, "*")
	if result == nil || result.Payload.NumRecords == nil || *result.Payload.NumRecords == 0 || result.Payload.AggregateResponseInlineRecords == nil {
		return nil, fmt.Errorf("could not retrieve aggregate information")
	}

	aggrList = make(map[string]string)

	for _, aggr := range result.Payload.AggregateResponseInlineRecords {
		if aggr.Name == nil {
			continue
		}
		if aggr.BlockStorage != nil && aggr.BlockStorage.Primary != nil && aggr.BlockStorage.Primary.DiskType != nil {
			aggrList[*aggr.Name] = *aggr.BlockStorage.Primary.DiskType
		}
	}

	return aggrList, nil
}

func (d OntapAPIREST) ExportPolicyDestroy(ctx context.Context, policy string) error {
	exportPolicyDestroyResult, err := d.api.ExportPolicyDestroy(ctx, policy)
	if err != nil {
		return fmt.Errorf("error deleting export policy: %v", err)
	}
	if exportPolicyDestroyResult == nil {
		return fmt.Errorf("error deleting export policy")
	}

	return err
}

func (d OntapAPIREST) VolumeExists(ctx context.Context, volumeName string) (bool, error) {
	return d.api.VolumeExists(ctx, volumeName)
}

func (d OntapAPIREST) TieringPolicyValue(ctx context.Context) string {
	return d.api.TieringPolicyValue(ctx)
}

func hasRestAggrSpaceInformation(ctx context.Context, aggrSpace *models.AggregateInlineSpace) bool {
	if aggrSpace == nil {
		return false
	}
	if aggrSpace.BlockStorage == nil {
		return false
	}
	if aggrSpace.BlockStorage.Size == nil {
		return false
	}
	if aggrSpace.BlockStorage.Used == nil {
		return false
	}
	if aggrSpace.Footprint == nil {
		return false
	}
	return true
}

func (d OntapAPIREST) GetSVMAggregateSpace(ctx context.Context, aggregate string) ([]SVMAggregateSpace, error) {
	response, aggrSpaceErr := d.api.AggregateList(ctx, aggregate)
	if aggrSpaceErr != nil {
		return nil, aggrSpaceErr
	}
	if response == nil {
		return nil, fmt.Errorf("error looking up aggregate: %v", aggregate)
	}

	if response.Payload == nil {
		return nil, fmt.Errorf("error looking up aggregate: %v", aggregate)
	}

	var svmAggregateSpaceList []SVMAggregateSpace

	for _, aggr := range response.Payload.AggregateResponseInlineRecords {

		if aggr == nil {
			Logc(ctx).Debugf("Skipping empty record")
			continue
		}

		if aggr.Name == nil {
			Logc(ctx).Debugf("Skipping empty record")
			continue
		}

		aggrName := *aggr.Name
		if aggregate != aggrName {
			Logc(ctx).Debugf("Skipping " + aggrName)
			continue
		}

		aggrSpace := aggr.Space
		if !hasRestAggrSpaceInformation(ctx, aggrSpace) {
			Logc(ctx).Debugf("Skipping entry with missing aggregate space information")
			continue
		}

		Logc(ctx).WithFields(LogFields{
			"aggrName": aggrName,
			"size":     aggrSpace.BlockStorage.Size,
		}).Info("Dumping aggregate space")

		// nil checked in hasRestAggrSpaceInformation()
		blockStorage := *aggrSpace.BlockStorage
		footprint := *aggrSpace.Footprint
		svmAggregateSpace := SVMAggregateSpace{
			size:      *blockStorage.Size,
			used:      *blockStorage.Used,
			footprint: footprint,
		}

		svmAggregateSpaceList = append(svmAggregateSpaceList, svmAggregateSpace)

	}

	return svmAggregateSpaceList, nil
}

func (d OntapAPIREST) VolumeDisableSnapshotDirectoryAccess(ctx context.Context, name string) error {
	if err := d.api.VolumeDisableSnapshotDirectoryAccess(ctx, name); err != nil {
		return fmt.Errorf("error disabling snapshot directory access: %v", err)
	}

	return nil
}

func (d OntapAPIREST) VolumeMount(ctx context.Context, name, junctionPath string) error {
	// Mount the volume at the specified junction
	if err := d.api.VolumeMount(ctx, name, junctionPath); err != nil {
		if restErr, ok := err.(RestError); ok {
			// Error code for expected error when mounting to DP volume that isn't initialized
			if restErr.code == DP_VOLUME_NOT_INITIALIZED {
				return ApiError(fmt.Sprintf("%v", err))
			}
		}
		return fmt.Errorf("error mounting volume %v to junction %v: %v", name, junctionPath, err)
	}

	return nil
}

func (d OntapAPIREST) VolumeRename(ctx context.Context, originalName, newName string) error {
	return d.api.VolumeRename(ctx, originalName, newName)
}

func (d OntapAPIREST) VolumeSetComment(
	ctx context.Context, volumeNameInternal, volumeNameExternal, comment string,
) error {
	if err := d.api.VolumeSetComment(ctx, volumeNameInternal, comment); err != nil {
		Logc(ctx).WithField("originalName", volumeNameExternal).Errorf("Modifying comment failed: %v", err)
		return fmt.Errorf("volume %s modify failed: %v", volumeNameExternal, err)
	}
	return nil
}

func (d OntapAPIREST) ExportPolicyCreate(ctx context.Context, policy string) error {
	// TODO use isExportPolicyExistsRest ?
	exportPolicy, err := d.api.ExportPolicyGetByName(ctx, policy)
	if err != nil {
		Logc(ctx).Errorf("error checking for existing export policy %s: %v", policy, err)
		return err
	}
	if exportPolicy != nil {
		// specified export policy already exists
		return nil
	}

	// could not find the specified export policy, create it
	policyCreateResponse, err := d.api.ExportPolicyCreate(ctx, policy)
	if err != nil {
		err = fmt.Errorf("error creating export policy %s: %v", policy, err)
	} else if policyCreateResponse == nil {
		err = fmt.Errorf("error creating export policy %s", policy)
	}

	return err
}

func (d OntapAPIREST) VolumeSize(ctx context.Context, volumeName string) (uint64, error) {
	return d.api.VolumeSize(ctx, volumeName)
}

func (d OntapAPIREST) VolumeUsedSize(ctx context.Context, volumeName string) (int, error) {
	return d.api.VolumeUsedSize(ctx, volumeName)
}

func (d OntapAPIREST) VolumeSetSize(ctx context.Context, name, newSize string) error {
	if err := d.api.VolumeSetSize(ctx, name, newSize); err != nil {
		Logc(ctx).WithField("error", err).Error("Volume resize failed.")
		return fmt.Errorf("volume resize failed")
	}

	return nil
}

func (d OntapAPIREST) VolumeModifyUnixPermissions(
	ctx context.Context, volumeNameInternal, volumeNameExternal, unixPermissions string,
) error {
	err := d.api.VolumeModifyUnixPermissions(ctx, volumeNameInternal, unixPermissions)
	if err != nil {
		Logc(ctx).WithField("originalName", volumeNameExternal).Errorf(
			"Could not import volume, modifying unix permissions failed: %v", err)
		return fmt.Errorf("volume %s modify failed: %v", volumeNameExternal, err)
	}

	return nil
}

func (d OntapAPIREST) VolumeListByPrefix(ctx context.Context, prefix string) (Volumes, error) {
	// TODO handle this higher? or just leave this here? i think here is OK
	if !strings.HasSuffix(prefix, "*") {
		// append the "*" to our prefix if it's missing
		prefix += "*"
	}

	volumesResponse, err := d.api.VolumeList(ctx, prefix)
	if err != nil {
		return nil, err
	}

	volumes := Volumes{}

	if volumesResponse.Payload != nil {
		payload := *volumesResponse.Payload
		for _, volume := range payload.VolumeResponseInlineRecords {
			volumeInfo, err := VolumeInfoFromRestAttrsHelper(volume)
			if err != nil {
				return nil, err
			}
			volumes = append(volumes, volumeInfo)
		}
	}

	return volumes, nil
}

// VolumeListByAttrs is used to find bucket volumes for nas-eco and san-eco
func (d OntapAPIREST) VolumeListByAttrs(ctx context.Context, volumeAttrs *Volume) (Volumes, error) {
	return d.api.VolumeListByAttrs(ctx, volumeAttrs)
}

func (d OntapAPIREST) ExportRuleCreate(ctx context.Context, policyName, desiredPolicyRules, nasProtocol string) error {
	var ruleResponse *n_a_s.ExportRuleCreateCreated
	var err error
	var protocol []string

	fields := LogFields{
		"Method":             "ExportRuleCreate",
		"Type":               "OntapAPIREST",
		"policyName":         policyName,
		"desiredPolicyRules": desiredPolicyRules,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> ExportRuleCreate")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< ExportRuleCreate")

	// unlike the ZAPI version of this function, we must create them 1 at a time here in REST
	for _, desiredPolicyRule := range strings.Split(desiredPolicyRules, ",") {
		if nasProtocol == sa.SMB {
			protocol = []string{"cifs"}
		} else {
			protocol = []string{"nfs"}
		}

		Logc(ctx).Debugf("processing desiredPolicyRule for %v protocol: '%v'", nasProtocol, desiredPolicyRule)
		ruleResponse, err = d.api.ExportRuleCreate(ctx, policyName, desiredPolicyRule, protocol, []string{"any"},
			[]string{"any"}, []string{"any"})
		if err != nil {
			err = fmt.Errorf("error creating export rule: %v", err)
			Logc(ctx).WithFields(LogFields{
				"ExportPolicy": policyName,
				"ClientMatch":  desiredPolicyRule,
			}).Error(err)
			return err
		}
		if ruleResponse == nil {
			return fmt.Errorf("unexpected response")
		}
	}

	return nil
}

func (d OntapAPIREST) ExportRuleDestroy(ctx context.Context, policyName string, ruleIndex int) error {
	ruleDestroyResponse, err := d.api.ExportRuleDestroy(ctx, policyName, ruleIndex)
	if err != nil {
		err = fmt.Errorf("error deleting export rule on policy %s at index %d; %v", policyName, ruleIndex, err)
		Logc(ctx).WithFields(LogFields{
			"ExportPolicy": policyName,
			"RuleIndex":    ruleIndex,
		}).Error(err)
	}

	if ruleDestroyResponse == nil {
		return fmt.Errorf("unexpected response")
	}
	return nil
}

func (d OntapAPIREST) VolumeModifyExportPolicy(ctx context.Context, volumeName, policyName string) error {
	err := d.api.VolumeModifyExportPolicy(ctx, volumeName, policyName)
	if err != nil {
		err = fmt.Errorf("error updating export policy on volume %s: %v", volumeName, err)
		Logc(ctx).Error(err)
		return err
	}

	return nil
}

func (d OntapAPIREST) ExportPolicyExists(ctx context.Context, policyName string) (bool, error) {
	policyGetResponse, err := d.api.ExportPolicyGetByName(ctx, policyName)
	if err != nil {
		err = fmt.Errorf("error getting export policy; %v", err)
		Logc(ctx).WithField("exportPolicy", policyName).Error(err)
		return false, err
	}
	if policyGetResponse == nil {
		return false, nil
	}
	return true, nil
}

func (d OntapAPIREST) ExportRuleList(ctx context.Context, policyName string) (map[string]int, error) {
	ruleListResponse, err := d.api.ExportRuleList(ctx, policyName)
	if err != nil {
		return nil, fmt.Errorf("error listing export policy rules: %v", err)
	}

	rules := make(map[string]int)
	if ruleListResponse != nil &&
		ruleListResponse.Payload != nil &&
		ruleListResponse.Payload.NumRecords != nil &&
		*ruleListResponse.Payload.NumRecords > 0 {

		exportRuleList := ruleListResponse.Payload.ExportRuleResponseInlineRecords
		for _, rule := range exportRuleList {
			for _, client := range rule.ExportRuleInlineClients {
				if client.Match != nil && rule.Index != nil {
					rules[*client.Match] = int(*rule.Index)
				}
			}
		}
	}

	return rules, nil
}

func (d OntapAPIREST) QtreeExists(ctx context.Context, name, volumePattern string) (bool, string, error) {
	return d.api.QtreeExists(ctx, name, volumePattern)
}

func (d OntapAPIREST) QtreeCreate(
	ctx context.Context, name, volumeName, unixPermissions, exportPolicy, securityStyle, qosPolicy string,
) error {
	return d.api.QtreeCreate(ctx, name, volumeName, unixPermissions, exportPolicy, securityStyle, qosPolicy)
}

func (d OntapAPIREST) QtreeDestroyAsync(ctx context.Context, path string, force bool) error {
	// REST interface doesn't use the /vol prefix in the paths
	path = strings.TrimPrefix(path, "/vol")
	return d.api.QtreeDestroyAsync(ctx, path, force)
}

func (d OntapAPIREST) QtreeRename(ctx context.Context, path, newPath string) error {
	// REST interface doesn't use the /vol prefix in the paths
	path = strings.TrimPrefix(path, "/vol")
	newPath = strings.TrimPrefix(newPath, "/vol")
	return d.api.QtreeRename(ctx, path, newPath)
}

func (d OntapAPIREST) QtreeModifyExportPolicy(ctx context.Context, name, volumeName, newExportPolicyName string) error {
	return d.api.QtreeModifyExportPolicy(ctx, name, volumeName, newExportPolicyName)
}

func (d OntapAPIREST) QtreeCount(ctx context.Context, volumeName string) (int, error) {
	return d.api.QtreeCount(ctx, volumeName)
}

func (d OntapAPIREST) QtreeListByPrefix(ctx context.Context, prefix, volumePrefix string) (Qtrees, error) {
	qtreeList, err := d.api.QtreeList(ctx, prefix, volumePrefix)
	if err != nil {
		msg := fmt.Sprintf("Error listing qtrees. %v", err)
		Logc(ctx).Errorf(msg)
		return nil, fmt.Errorf(msg)
	}
	qtrees := Qtrees{}
	for _, qtree := range qtreeList.GetPayload().QtreeResponseInlineRecords {
		newQtree := d.convertQtree(qtree)
		qtrees = append(qtrees, newQtree)
	}

	return qtrees, nil
}

func (d OntapAPIREST) convertQtree(qtree *models.Qtree) *Qtree {
	newQtree := &Qtree{}

	if qtree.Name != nil {
		newQtree.Name = *qtree.Name
	}
	if qtree.SecurityStyle != nil {
		newQtree.SecurityStyle = string(*qtree.SecurityStyle)
	}
	if qtree.UnixPermissions != nil {
		newQtree.UnixPermissions = strconv.FormatInt(*qtree.UnixPermissions, 10)
	}
	if qtree.ExportPolicy != nil && qtree.ExportPolicy.Name != nil {
		newQtree.ExportPolicy = *qtree.ExportPolicy.Name
	}
	if qtree.Volume != nil && qtree.Volume.Name != nil {
		newQtree.Volume = *qtree.Volume.Name
	}
	if qtree.Svm != nil && qtree.Svm.Name != nil {
		newQtree.Vserver = *qtree.Svm.Name
	}
	return newQtree
}

func (d OntapAPIREST) QtreeGetByName(ctx context.Context, name, volumePrefix string) (*Qtree, error) {
	qtree, err := d.api.QtreeGet(ctx, name, volumePrefix)
	if err != nil {
		msg := "error getting qtree"
		Logc(ctx).WithError(err).Errorf(msg)
		return nil, fmt.Errorf(msg)
	}
	return d.convertQtree(qtree), nil
}

func (d OntapAPIREST) QuotaEntryList(ctx context.Context, volumeName string) (QuotaEntries, error) {
	response, err := d.api.QuotaEntryList(ctx, volumeName)
	if err != nil {
		return nil, err
	}
	entries := QuotaEntries{}
	if response != nil && response.Payload != nil {
		for _, entry := range response.Payload.QuotaRuleResponseInlineRecords {
			entries = append(entries, d.convertQuota(entry))
		}
	}
	return entries, nil
}

func (d OntapAPIREST) QuotaOff(ctx context.Context, volumeName string) error {
	err := d.api.QuotaOff(ctx, volumeName)
	if err != nil {
		msg := "error disabling quota"
		Logc(ctx).WithError(err).WithField("volume", volumeName).Error(msg)
		return err
	}
	return nil
}

func (d OntapAPIREST) QuotaOn(ctx context.Context, volumeName string) error {
	err := d.api.QuotaOn(ctx, volumeName)
	if err != nil {
		msg := "error enabling quota"
		Logc(ctx).WithError(err).WithField("volume", volumeName).Error(msg)
		return err
	}
	return nil
}

func (d OntapAPIREST) QuotaResize(context.Context, string) error {
	// With REST Changes to quota rule limits ("space.hard_limit", "space.soft_limit", "files.hard_limit",
	// and "files.soft_limit") are applied automatically without requiring a quota resize operation.
	return nil
}

func (d OntapAPIREST) QuotaStatus(ctx context.Context, volumeName string) (string, error) {
	volume, err := d.api.VolumeGetByName(ctx, volumeName)
	if err != nil {
		return "", fmt.Errorf("error getting quota status for Flexvol %s: %v", volumeName, err)
	}

	if volume.Quota == nil {
		return "", fmt.Errorf("error getting quota status for Flexvol %s: %v", volumeName, err)
	}

	if volume.Quota.State == nil {
		return "", fmt.Errorf("error getting quota status for Flexvol %s: %v", volumeName, err)
	}

	return *volume.Quota.State, nil
}

func (d OntapAPIREST) QuotaSetEntry(ctx context.Context, qtreeName, volumeName, quotaType, diskLimit string) error {
	if diskLimit == "" {
		diskLimit = "-1"
	} else {
		hardLimit, parseErr := strconv.ParseInt(diskLimit, 10, 64)
		if parseErr != nil {
			return fmt.Errorf("cannot process disk limit value %v", diskLimit)
		}
		hardLimit *= 1024 // REST takes the limit in bytes, not KB
		diskLimit = strconv.FormatInt(hardLimit, 10)
	}
	return d.api.QuotaSetEntry(ctx, qtreeName, volumeName, quotaType, diskLimit)
}

func (d OntapAPIREST) QuotaGetEntry(ctx context.Context, volumeName, qtreeName, quotaType string) (*QuotaEntry, error) {
	quota, err := d.api.QuotaGetEntry(ctx, volumeName, qtreeName, quotaType)
	if err != nil {
		Logc(ctx).WithError(err).Error("error getting quota rule")
	}
	quotaEntry := d.convertQuota(quota)
	return quotaEntry, nil
}

func (d OntapAPIREST) convertQuota(quota *models.QuotaRule) *QuotaEntry {
	diskLimit := int64(-1)
	if quota.Space != nil && quota.Space.HardLimit != nil {
		diskLimit = *quota.Space.HardLimit
	}
	quotaEntry := &QuotaEntry{
		DiskLimitBytes: diskLimit,
	}
	if quota != nil && quota.Volume != nil && quota.Volume.Name != nil && *quota.Volume.Name != "" {
		quotaEntry.Target = fmt.Sprintf("/vol/%s", *quota.Volume.Name)

		if quota.Qtree != nil && quota.Qtree.Name != nil && *quota.Qtree.Name != "" {
			// append
			quotaEntry.Target += fmt.Sprintf("/%s", *quota.Qtree.Name)
		}
	}
	return quotaEntry
}

func (d OntapAPIREST) VolumeSnapshotCreate(ctx context.Context, snapshotName, sourceVolume string) error {
	volume, err := d.VolumeInfo(ctx, sourceVolume)
	if err != nil {
		return fmt.Errorf("error looking up source volume %v: %v", sourceVolume, err)
	}
	if volume == nil {
		return fmt.Errorf("error looking up source volume: %v", sourceVolume)
	}

	if err = d.api.SnapshotCreateAndWait(ctx, volume.UUID, snapshotName); err != nil {
		return fmt.Errorf("could not create snapshot: %v", err)
	}
	return nil
}

// pollVolumeExistence polls for the volume, with backoff retry logic
func (d OntapAPIREST) pollVolumeExistence(ctx context.Context, volumeName string) error {
	checkVolumeStatus := func() error {
		volume, err := d.VolumeInfo(ctx, volumeName)
		if err != nil {
			return err
		}
		if volume == nil {
			return fmt.Errorf("could not find Volume with name %v", volumeName)
		}
		return nil
	}
	volumeStatusNotify := func(err error, duration time.Duration) {
		Logc(ctx).WithField("increment", duration).Debug("Volume not found, waiting.")
	}
	volumeStatusBackoff := backoff.NewExponentialBackOff()
	volumeStatusBackoff.InitialInterval = 1 * time.Second
	volumeStatusBackoff.Multiplier = 2
	volumeStatusBackoff.RandomizationFactor = 0.1
	volumeStatusBackoff.MaxElapsedTime = 2 * time.Minute

	// Run the check using an exponential backoff
	if err := backoff.RetryNotify(checkVolumeStatus, volumeStatusBackoff, volumeStatusNotify); err != nil {
		Logc(ctx).WithField("Volume", volumeName).Warnf("Volume not found after %3.2f seconds.",
			volumeStatusBackoff.MaxElapsedTime.Seconds())
		return err
	}

	return nil
}

func (d OntapAPIREST) VolumeCloneCreate(ctx context.Context, cloneName, sourceName, snapshot string, async bool) error {
	err := d.api.VolumeCloneCreateAsync(ctx, cloneName, sourceName, snapshot)
	if err != nil {
		return fmt.Errorf("error creating clone: %v", err)
	}

	return nil
}

func (d OntapAPIREST) VolumeSnapshotList(ctx context.Context, sourceVolume string) (Snapshots, error) {
	volume, err := d.VolumeInfo(ctx, sourceVolume)
	if err != nil {
		return nil, fmt.Errorf("error looking up source volume: %v", err)
	}
	if volume == nil {
		return nil, fmt.Errorf("error looking up source volume: %v", sourceVolume)
	}

	snapListResponse, err := d.api.SnapshotList(ctx, volume.UUID)
	if err != nil {
		return nil, fmt.Errorf("error enumerating snapshots: %v", err)
	}
	if snapListResponse == nil {
		return nil, fmt.Errorf("error enumerating snapshots")
	}
	snapshots := Snapshots{}

	if snapListResponse.Payload != nil {
		for _, snap := range snapListResponse.Payload.SnapshotResponseInlineRecords {
			if snap.CreateTime == nil || snap.Name == nil {
				continue
			}
			snapshots = append(snapshots, Snapshot{
				CreateTime: snap.CreateTime.String(),
				Name:       *snap.Name,
			})
		}
	}

	Logc(ctx).Debugf("Returned %v snapshots.", snapListResponse.Payload.NumRecords)

	return snapshots, nil
}

func (d OntapAPIREST) VolumeSetQosPolicyGroupName(ctx context.Context, name string, qos QosPolicyGroup) error {
	if err := d.api.VolumeSetQosPolicyGroupName(ctx, name, qos); err != nil {
		return fmt.Errorf("error setting quality of service policy: %v", err)
	}

	return nil
}

func (d OntapAPIREST) VolumeCloneSplitStart(ctx context.Context, cloneName string) error {
	if err := d.api.VolumeCloneSplitStart(ctx, cloneName); err != nil {
		return fmt.Errorf("error splitting clone: %v", err)
	}
	return nil
}

func (d OntapAPIREST) SnapshotRestoreVolume(
	ctx context.Context, snapshotName, sourceVolume string,
) error {
	if err := d.api.SnapshotRestoreVolume(ctx, snapshotName, sourceVolume); err != nil {
		return fmt.Errorf("error restoring snapshot: %v", err)
	}

	return nil
}

func (d OntapAPIREST) SnapshotRestoreFlexgroup(ctx context.Context, snapshotName, sourceVolume string) error {
	if err := d.api.SnapshotRestoreFlexgroup(ctx, snapshotName, sourceVolume); err != nil {
		return fmt.Errorf("error restoring snapshot: %v", err)
	}

	return nil
}

func (d OntapAPIREST) SnapshotDeleteByNameAndStyle(
	ctx context.Context, snapshotName, sourceVolume, sourceVolumeUUID string,
) error {
	// GET the snapshot by name
	snapshot, err := d.api.SnapshotGetByName(ctx, sourceVolumeUUID, snapshotName)
	if err != nil {
		return fmt.Errorf("error checking for snapshot: %v", err)
	}
	if snapshot == nil {
		return fmt.Errorf("error looking up snapshot: %v", snapshotName)
	}
	if snapshot.UUID == nil {
		return fmt.Errorf("error looking up snapshot: %v", snapshotName)
	}
	snapshotUUID := *snapshot.UUID

	// DELETE the snapshot
	snapshotDeleteResult, err := d.api.SnapshotDelete(ctx, sourceVolumeUUID, snapshotUUID)
	if err != nil {
		return fmt.Errorf("error while deleting snapshot: %v", err)
	}
	if snapshotDeleteResult == nil {
		return fmt.Errorf("error while deleting snapshot: %v", snapshotName)
	}

	// check snapshot delete job status
	snapshotDeleteErr := d.api.PollJobStatus(ctx, snapshotDeleteResult.Payload)
	// if err := client.PollJobStatus(ctx, snapshotDeleteResult.Payload); err != nil {
	if snapshotDeleteErr != nil {
		Logc(ctx).Debugf("Could not delete the snapshot, going to check if it's busy; error was: %v", snapshotDeleteErr)
		if restErr, ok := snapshotDeleteErr.(RestError); ok {
			Logc(ctx).Debugf("restErr: %v", restErr)
			Logc(ctx).Debugf("restErr.Code(): %v", restErr.Code())
			if restErr.IsSnapshotBusy() {
				Logc(ctx).Debug("Snapshot was busy, going to split it")
				// Start a split here before returning the error so a subsequent delete attempt may succeed.
				return SnapshotBusyError(fmt.Sprintf("snapshot %s backing volume %s is busy", snapshotName,
					sourceVolume))
			}
		}
		return snapshotDeleteErr
	}

	return nil
}

func (d OntapAPIREST) FlexgroupSnapshotDelete(ctx context.Context, snapshotName, sourceVolume string) error {
	volume, err := d.FlexgroupInfo(ctx, sourceVolume)
	if err != nil {
		return fmt.Errorf("error looking up source volume: %v", err)
	}
	if volume == nil {
		return fmt.Errorf("error looking up source volume: %v", sourceVolume)
	}
	volumeUUID := volume.UUID

	return d.SnapshotDeleteByNameAndStyle(ctx, snapshotName, sourceVolume, volumeUUID)
}

func (d OntapAPIREST) VolumeSnapshotDelete(ctx context.Context, snapshotName, sourceVolume string) error {
	volume, err := d.VolumeInfo(ctx, sourceVolume)
	if err != nil {
		return fmt.Errorf("error looking up source volume: %v", err)
	}
	if volume == nil {
		return fmt.Errorf("error looking up source volume: %v", sourceVolume)
	}
	volumeUUID := volume.UUID

	return d.SnapshotDeleteByNameAndStyle(ctx, snapshotName, sourceVolume, volumeUUID)
}

func (d OntapAPIREST) VolumeListBySnapshotParent(
	ctx context.Context, snapshotName, sourceVolume string,
) (VolumeNameList, error) {
	childVolumes, err := d.api.VolumeListAllBackedBySnapshot(ctx, sourceVolume, snapshotName)
	if err != nil {
		Logc(ctx).WithFields(LogFields{
			"snapshotName":     snapshotName,
			"parentVolumeName": sourceVolume,
			"error":            err,
		}).Error("Could not list volumes backed by snapshot.")
		return nil, err
	} else if len(childVolumes) == 0 {
		return nil, nil
	}

	// We're going to start a single split operation, but there could be multiple children, so we
	// sort the volumes by name to not have more than one split operation running at a time.
	sort.Strings(childVolumes)

	return childVolumes, nil
}

// ////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// BEGIN: Snapmirror operations
// ////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

func (d OntapAPIREST) SnapmirrorDeleteViaDestination(
	ctx context.Context, localInternalVolumeName, localSVMName string,
) error {
	err := d.api.SnapmirrorDeleteViaDestination(ctx, localInternalVolumeName, localSVMName)
	if err != nil {
		if !IsNotFoundError(err) {
			return fmt.Errorf("error deleting snapmirror info for volume %v: %v", localInternalVolumeName, err)
		}
	}

	// Ensure no leftover snapmirror metadata
	err = d.api.SnapmirrorRelease(ctx, localInternalVolumeName, localSVMName)
	if err != nil {
		if !IsNotFoundError(err) {
			return fmt.Errorf("error releasing snapmirror info for volume %v: %v", localInternalVolumeName, err)
		}
	}

	return nil
}

func (d OntapAPIREST) SnapmirrorRelease(ctx context.Context, sourceFlexvolName, sourceSVMName string) error {
	// Ensure no leftover snapmirror metadata
	err := d.api.SnapmirrorRelease(ctx, sourceFlexvolName, sourceSVMName)
	if err != nil {
		return fmt.Errorf("error releasing snapmirror info for volume %v: %v", sourceFlexvolName, err)
	}

	return nil
}

func (d OntapAPIREST) IsSVMDRCapable(ctx context.Context) (bool, error) {
	return d.api.IsVserverDRCapable(ctx)
}

func (d OntapAPIREST) SnapmirrorCreate(
	ctx context.Context, localInternalVolumeName, localSVMName, remoteFlexvolName,
	remoteSVMName, replicationPolicy, replicationSchedule string,
) error {
	return d.api.SnapmirrorCreate(ctx, localInternalVolumeName, localSVMName, remoteFlexvolName, remoteSVMName,
		replicationPolicy, replicationSchedule)
}

func (d OntapAPIREST) SnapmirrorGet(
	ctx context.Context, localInternalVolumeName, localSVMName, remoteFlexvolName,
	remoteSVMName string,
) (*Snapmirror, error) {
	snapmirrorResponse, err := d.api.SnapmirrorGet(ctx, localInternalVolumeName, localSVMName, remoteFlexvolName,
		remoteSVMName)
	if err != nil {
		return nil, err
	}
	if snapmirrorResponse == nil {
		return nil, fmt.Errorf("unexpected error on snapmirror get")
	}

	snapmirror := &Snapmirror{}

	if snapmirrorResponse.State != nil {
		snapmirror.State = SnapmirrorState(*snapmirrorResponse.State)
	}

	if snapmirrorResponse.LastTransferType != nil {
		snapmirror.LastTransferType = *snapmirrorResponse.LastTransferType
	}

	if snapmirrorResponse.Transfer != nil && snapmirrorResponse.Transfer.State != nil {
		snapmirror.RelationshipStatus = SnapmirrorStatus(*snapmirrorResponse.Transfer.State)
	}

	if snapmirrorResponse.Healthy != nil {
		snapmirror.IsHealthy = *snapmirrorResponse.Healthy
		if !snapmirror.IsHealthy {
			unhealthyReason := snapmirrorResponse.SnapmirrorRelationshipInlineUnhealthyReason
			if len(unhealthyReason) > 0 {
				snapmirror.UnhealthyReason = *unhealthyReason[0].Message
			}
		}
	}

	if snapmirrorResponse.Policy != nil && snapmirrorResponse.Policy.Name != nil {
		snapmirror.ReplicationPolicy = *snapmirrorResponse.Policy.Name
	}

	if snapmirrorResponse.TransferSchedule != nil && snapmirrorResponse.TransferSchedule.Name != nil {
		snapmirror.ReplicationSchedule = *snapmirrorResponse.TransferSchedule.Name
	}

	return snapmirror, nil
}

func (d OntapAPIREST) SnapmirrorInitialize(
	ctx context.Context, localInternalVolumeName, localSVMName, remoteFlexvolName,
	remoteSVMName string,
) error {
	err := d.api.SnapmirrorInitialize(ctx, localInternalVolumeName, localSVMName, remoteFlexvolName, remoteSVMName)
	if err != nil {
		if restErr, err := ExtractErrorResponse(ctx, err); err == nil {
			if restErr.Error != nil && restErr.Error.Code != nil && *restErr.Error.Code == SNAPMIRROR_TRANSFER_IN_PROGRESS {
				Logc(ctx).Debug("snapmirror transfer already in progress")
				return nil
			}
		}
		Logc(ctx).WithError(err).Error("Error on snapmirror initialize")
		return err
	}
	return nil
}

func (d OntapAPIREST) SnapmirrorDelete(
	ctx context.Context, localInternalVolumeName, localSVMName, remoteFlexvolName,
	remoteSVMName string,
) error {
	return d.api.SnapmirrorDelete(ctx, localInternalVolumeName, localSVMName, remoteFlexvolName, remoteSVMName)
}

func (d OntapAPIREST) SnapmirrorResync(
	ctx context.Context, localInternalVolumeName, localSVMName, remoteFlexvolName,
	remoteSVMName string,
) error {
	err := d.api.SnapmirrorResync(ctx, localInternalVolumeName, localSVMName, remoteFlexvolName, remoteSVMName)
	if err != nil {
		Logc(ctx).WithError(err).Error("Error on snapmirror resync")
		// If we fail on the resync, we need to cleanup the snapmirror
		// it will be recreated in a future TMR reconcile loop through this function
		if delError := d.SnapmirrorDelete(ctx, localInternalVolumeName, localSVMName, remoteFlexvolName,
			remoteSVMName); delError != nil {
			Logc(ctx).WithError(delError).Error("Error on snapmirror delete following a resync failure")
		}
		return err
	}
	return nil
}

func (d OntapAPIREST) SnapmirrorPolicyGet(ctx context.Context, replicationPolicy string) (*SnapmirrorPolicy, error) {
	snapmirrorPolicyResponse, err := d.api.SnapmirrorPolicyGet(ctx, replicationPolicy)
	if err != nil {
		return nil, err
	}

	if snapmirrorPolicyResponse == nil || snapmirrorPolicyResponse.Payload == nil {
		return nil, fmt.Errorf("unexpected error on snapmirror policy get")
	}

	if len(snapmirrorPolicyResponse.Payload.SnapmirrorPolicyResponseInlineRecords) != 1 {
		return nil, fmt.Errorf("unexpected error on snapmirror policy get")
	}

	response := snapmirrorPolicyResponse.Payload.SnapmirrorPolicyResponseInlineRecords[0]
	if response == nil || response.Type == nil {
		return nil, fmt.Errorf("unexpected error on snapmirror policy get")
	}

	var syncType SnapmirrorPolicyType
	if SnapmirrorPolicyType(*response.Type).IsSnapmirrorPolicyTypeSync() {
		if response.SyncType == nil {
			return nil, fmt.Errorf("unexpected error on snapmirror policy get")
		}
		syncType = SnapmirrorPolicyType(*response.SyncType)
	} else {
		syncType = SnapmirrorPolicyType(*response.Type)
	}

	copyAllSnapshots := false
	if response.CopyAllSourceSnapshots != nil {
		copyAllSnapshots = *response.CopyAllSourceSnapshots
	}

	snapmirrorPolicy := &SnapmirrorPolicy{
		Type:             syncType,
		CopyAllSnapshots: copyAllSnapshots,
	}

	return snapmirrorPolicy, nil
}

func (d OntapAPIREST) isTransferInProgressError(
	ctx context.Context, err error,
) bool {
	if restErr, err := ExtractErrorResponse(ctx, err); err == nil {
		if restErr.Error != nil && restErr.Error.Code != nil {
			switch *restErr.Error.Code {
			case SNAPMIRROR_TRANSFER_IN_PROGRESS, SNAPMIRROR_TRANSFER_IN_PROGRESS_BROKEN_OFF:
				return true
			default:
				return false
			}
		}
	}
	return false
}

func (d OntapAPIREST) SnapmirrorQuiesce(
	ctx context.Context, localInternalVolumeName, localSVMName, remoteFlexvolName,
	remoteSVMName string,
) error {
	err := d.api.SnapmirrorQuiesce(ctx, localInternalVolumeName, localSVMName, remoteFlexvolName, remoteSVMName)
	if err != nil {
		if d.isTransferInProgressError(ctx, err) {
			msg := "snapmirror transfer already in progress"
			Logc(ctx).Debug(msg)
			return NotReadyError(fmt.Sprintf("Snapmirror quiesce failed: %s", msg))
		}
		Logc(ctx).WithError(err).Error("Error on snapmirror quiesce")
		return err
	}
	return nil
}

func (d OntapAPIREST) SnapmirrorAbort(
	ctx context.Context, localInternalVolumeName, localSVMName, remoteFlexvolName,
	remoteSVMName string,
) error {
	err := d.api.SnapmirrorAbort(ctx, localInternalVolumeName, localSVMName, remoteFlexvolName, remoteSVMName)
	if err != nil {
		if d.isTransferInProgressError(ctx, err) {
			msg := "snapmirror transfer already in progress"
			Logc(ctx).Debug(msg)
			return NotReadyError(fmt.Sprintf("Snapmirror abort failed: %s", msg))
		}
		Logc(ctx).WithError(err).Error("Error on snapmirror abort")
		return err
	}
	return nil
}

func (d OntapAPIREST) SnapmirrorBreak(
	ctx context.Context, localInternalVolumeName, localSVMName, remoteFlexvolName,
	remoteSVMName, snapshotName string,
) error {
	// TODO: potential error if volume is not DP
	err := d.api.SnapmirrorBreak(ctx, localInternalVolumeName, localSVMName, remoteFlexvolName, remoteSVMName,
		snapshotName)
	if err != nil {
		if d.isTransferInProgressError(ctx, err) {
			msg := "snapmirror transfer already in progress"
			Logc(ctx).Debug(msg)
			return NotReadyError(fmt.Sprintf("Snapmirror break failed: %s", msg))
		}
		Logc(ctx).WithError(err).Error("Error on snapmirror break")
		return err
	}
	return nil
}

// ////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
// END: Snapmirror operations
// ////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

func (d OntapAPIREST) JobScheduleExists(ctx context.Context, replicationSchedule string) (bool, error) {
	return d.api.JobScheduleExists(ctx, replicationSchedule)
}

func (d OntapAPIREST) GetSVMPeers(ctx context.Context) ([]string, error) {
	return d.api.GetPeeredVservers(ctx)
}

func (d OntapAPIREST) LunList(ctx context.Context, pattern string) (Luns, error) {
	lunsResponse, err := d.api.LunList(ctx, pattern)
	if err != nil {
		return nil, err
	}

	luns := Luns{}

	if lunsResponse.Payload != nil {
		payload := *lunsResponse.Payload
		for _, lun := range payload.LunResponseInlineRecords {
			lunInfo, err := lunInfoFromRestAttrsHelper(lun)
			if err != nil {
				return nil, err
			}
			luns = append(luns, *lunInfo)
		}
	}

	return luns, nil
}

func (d OntapAPIREST) LunCreate(ctx context.Context, lun Lun) error {
	fields := LogFields{
		"Method": "LunCreate",
		"Type":   "OntapAPIREST",
		"spec":   lun,
	}
	Logd(ctx, d.driverName, d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> LunCreate")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< LunCreate")

	sizeBytesStr, _ := utils.ConvertSizeToBytes(lun.Size)
	sizeBytes, _ := strconv.ParseUint(sizeBytesStr, 10, 64)
	creationErr := d.api.LunCreate(ctx, lun.Name, int64(sizeBytes), lun.OsType, lun.Qos, *lun.SpaceReserved,
		*lun.SpaceAllocated)
	if creationErr != nil {
		return fmt.Errorf("error creating LUN %v: %v", lun.Name, creationErr)
	}

	return nil
}

func (d OntapAPIREST) LunDestroy(ctx context.Context, lunPath string) error {
	fields := LogFields{
		"Method": "LunDestroy",
		"Type":   "OntapAPIREST",
		"Name":   lunPath,
	}
	Logd(ctx, d.driverName, d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> LunDestroy")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< LunDestroy")

	lun, err := d.api.LunGetByName(ctx, lunPath)
	if err != nil {
		return fmt.Errorf("error getting LUN: %v", lunPath)
	}
	if lun.UUID == nil {
		return fmt.Errorf("error getting LUN: %v", lunPath)
	}

	err = d.api.LunDelete(ctx, *lun.UUID)
	if err != nil {
		return fmt.Errorf("error deleting LUN: %v", lunPath)
	}

	return nil
}

func (d OntapAPIREST) LunGetGeometry(ctx context.Context, lunPath string) (uint64, error) {
	lunOptionsResult, err := d.api.LunOptions(ctx)
	if err != nil {
		return 0, fmt.Errorf("error get lun options for LUN: %v, err: %d", lunPath, err)
	}
	if lunOptionsResult == nil {
		return 0, fmt.Errorf("lun options for LUN: %v are nil", lunPath)
	}

	return uint64(lunOptionsResult.RecordSchema.Space.Size.Range.Max), nil
}

func (d OntapAPIREST) LunSetAttribute(ctx context.Context, lunPath, attribute, fstype, context, luks string) error {
	if strings.Contains(lunPath, failureLUNSetAttr) {
		return errors.New("injected error")
	}

	if err := d.api.LunSetAttribute(ctx, lunPath, attribute, fstype); err != nil {
		Logc(ctx).WithField("LUN", lunPath).Error("Failed to save the fstype attribute for new LUN.")
		return err
	}

	if context != "" {
		if err := d.api.LunSetAttribute(ctx, lunPath, "context", context); err != nil {
			Logc(ctx).WithField("LUN", lunPath).Warning("Failed to save the driver context attribute for new LUN.")
		}
	}

	if luks != "" {
		if err := d.api.LunSetAttribute(ctx, lunPath, "LUKS", luks); err != nil {
			Logc(ctx).WithField("LUN", lunPath).Warning("Failed to save the LUKS attribute for new LUN.")
		}
	}

	return nil
}

func (d OntapAPIREST) LunGetFSType(ctx context.Context, lunPath string) (string, error) {
	// Get the fstype from LUN Attribute
	LUNAttributeFSType := "com.netapp.ndvp.fstype"
	fstype, err := d.api.LunGetAttribute(ctx, lunPath, LUNAttributeFSType)
	if err != nil {
		// If not found, extract the fstype from LUN Comment
		comment, err := d.api.LunGetComment(ctx, lunPath)
		if err != nil {
			return "", err
		}

		// Parse the comment to get fstype value
		var lunComment map[string]map[string]string
		err = json.Unmarshal([]byte(comment), &lunComment)
		if err != nil {
			return "", err
		}
		lunAttrs := lunComment["lunAttributes"]
		if lunAttrs != nil {
			fstype = lunAttrs["fstype"]
		} else {
			return "", fmt.Errorf("lunAttributes field not found in LUN comment")
		}
	}

	Logc(ctx).WithFields(LogFields{"LUN": lunPath, "fstype": fstype}).Debug("Found LUN attribute fstype.")
	return fstype, nil
}

func (d OntapAPIREST) LunCloneCreate(
	ctx context.Context, flexvol, source, lunPath string,
	qosPolicyGroup QosPolicyGroup,
) error {
	fullSourceLunPath := source
	if !strings.HasPrefix(source, "/vol/") {
		fullSourceLunPath = fmt.Sprintf("/vol/%s/%s", flexvol, source)
	}

	fullCloneLunPath := lunPath
	if !strings.HasPrefix(lunPath, "/vol/") {
		fullCloneLunPath = fmt.Sprintf("/vol/%s/%s", flexvol, lunPath)
	}

	fields := LogFields{
		"Method":            "LunCloneCreate",
		"Type":              "OntapAPIREST",
		"flexvol":           flexvol,
		"source":            source,
		"lunPath":           lunPath,
		"fullSourceLunPath": fullSourceLunPath,
		"fullCloneLunPath":  fullCloneLunPath,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> LunCloneCreate")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< LunCloneCreate")

	lunResponse, err := d.api.LunGetByName(ctx, fullSourceLunPath)
	if err != nil {
		return err
	}
	lun, err := lunInfoFromRestAttrsHelper(lunResponse)
	if err != nil {
		return err
	}

	sizeBytesStr, _ := utils.ConvertSizeToBytes(lun.Size)
	sizeBytes, _ := strconv.ParseInt(sizeBytesStr, 10, 64)

	return d.api.LunCloneCreate(ctx, fullCloneLunPath, fullSourceLunPath, sizeBytes, lun.OsType, qosPolicyGroup)
}

// TODO: Change this for LUN Attributes when available
func (d OntapAPIREST) LunSetComments(ctx context.Context, lunPath, fstype, context, luks string) error {
	setComment, err := d.GetCommentJSON(ctx, fstype, context, luks, 254)
	if err != nil {
		return err
	}
	return d.api.LunSetComment(ctx, lunPath, setComment)
}

// TODO: Change this for LUN Attributes when available
// GetCommentJSON returns a JSON-formatted string containing the labels on this LUN.
// This is a temporary solution until we are able to implement LUN attributes in REST
// For example: {"lunAttributes":{"fstype":"xfs","driverContext":"csi"}}
func (d OntapAPIREST) GetCommentJSON(ctx context.Context, fstype, context, luks string, commentLimit int) (string,
	error,
) {
	lunCommentMap := make(map[string]map[string]string)
	newcommentMap := make(map[string]string)
	newcommentMap["fstype"] = fstype
	newcommentMap["LUKS"] = luks
	newcommentMap["driverContext"] = context
	lunCommentMap["lunAttributes"] = newcommentMap

	lunCommentJSON, err := json.Marshal(lunCommentMap)
	if err != nil {
		Logc(ctx).Errorf("Failed to marshal lun comments: %+v", lunCommentMap)
		return "", err
	}

	commentsJsonBytes := new(bytes.Buffer)
	err = json.Compact(commentsJsonBytes, lunCommentJSON)
	if err != nil {
		Logc(ctx).Errorf("Failed to compact lun comments: %s", string(lunCommentJSON))
		return "", err
	}

	commentsJSON := commentsJsonBytes.String()

	if commentLimit != 0 && len(commentsJSON) > commentLimit {
		Logc(ctx).WithFields(LogFields{
			"commentsJSON":       commentsJSON,
			"commentsJSONLength": len(commentsJSON),
			"maxCommentLength":   commentLimit,
		}).Error("comment length exceeds the character limit")
		return "", fmt.Errorf("comment length %v exceeds the character limit of %v characters", len(commentsJSON),
			commentLimit)
	}

	return commentsJSON, nil
}

func (d OntapAPIREST) LunSetQosPolicyGroup(ctx context.Context, lunPath string, qosPolicyGroup QosPolicyGroup) error {
	return d.api.LunSetQosPolicyGroup(ctx, lunPath, qosPolicyGroup.Name)
}

func (d OntapAPIREST) LunGetByName(ctx context.Context, name string) (*Lun, error) {
	fields := LogFields{
		"Method":  "LunGetByName",
		"Type":    "OntapAPIREST",
		"LunPath": name,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> LunGetByName")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< LunGetByName")

	lunResponse, err := d.api.LunGetByName(ctx, name)
	if err != nil {
		return nil, err
	}
	lun, err := lunInfoFromRestAttrsHelper(lunResponse)
	if err != nil {
		return nil, err
	}
	return lun, nil
}

func (d OntapAPIREST) LunRename(ctx context.Context, lunPath, newLunPath string) error {
	fields := LogFields{
		"Method":     "LunRename",
		"Type":       "OntapAPIREST",
		"OldLunName": lunPath,
		"NewLunName": newLunPath,
	}
	Logd(ctx, d.driverName, d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> LunRename")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< LunRename")

	return d.api.LunRename(ctx, lunPath, newLunPath)
}

func (d OntapAPIREST) LunMapInfo(ctx context.Context, initiatorGroupName, lunPath string) (int, error) {
	lunID := -1
	info, err := d.api.LunMapInfo(ctx, initiatorGroupName, lunPath)
	if err != nil {
		return lunID, fmt.Errorf("error reading LUN maps for volume %s: %v", lunPath, err)
	}

	if info.Payload != nil {
		for _, lunMapResponse := range info.Payload.LunMapResponseInlineRecords {
			if lunMapResponse.Igroup != nil &&
				lunMapResponse.Igroup.Name != nil &&
				*lunMapResponse.Igroup.Name == initiatorGroupName {
				if lunMapResponse.LogicalUnitNumber != nil {
					lunID = int(*lunMapResponse.LogicalUnitNumber)
				}
			}
		}
	}
	return lunID, nil
}

func (d OntapAPIREST) isLunMapped(
	ctx context.Context, lunPath, initiatorGroupName string, importNotManaged bool,
) (bool, int, error) {
	alreadyMapped := false
	lunID := -1

	lunMapResponse, err := d.api.LunMapInfo(ctx, "", lunPath)
	if err != nil {
		return alreadyMapped, lunID, fmt.Errorf("problem reading maps for LUN %s: %v", lunPath, err)
	}
	if lunMapResponse == nil || lunMapResponse.Payload == nil {
		return alreadyMapped, lunID, fmt.Errorf("problem reading maps for LUN %s", lunPath)
	}

	Logc(ctx).WithFields(
		LogFields{
			"lun":    lunPath,
			"igroup": initiatorGroupName,
		},
	).Debug("Checking if LUN is mapped to igroup.")

	for _, record := range lunMapResponse.Payload.LunMapResponseInlineRecords {
		if record.Igroup != nil && record.Igroup.Name != nil {
			if *record.Igroup.Name != initiatorGroupName {
				Logc(ctx).Debugf("LUN %s is mapped to igroup %s.", lunPath, record.Igroup.Name)
			}
			if *record.Igroup.Name == initiatorGroupName || importNotManaged {
				if record.LogicalUnitNumber != nil {
					lunID = int(*record.LogicalUnitNumber)
					alreadyMapped = true

					Logc(ctx).WithFields(
						LogFields{
							"lun":    lunPath,
							"igroup": initiatorGroupName,
							"id":     lunID,
						},
					).Debug("LUN already mapped.")
					break
				} else {
					lun, err := d.api.LunGetByName(ctx, lunPath)
					if err != nil {
						return alreadyMapped, lunID, err
					}
					if lun != nil && len(lun.LunInlineLunMaps) > 0 && lun.LunInlineLunMaps[0].LogicalUnitNumber != nil {
						lunID = int(*lun.LunInlineLunMaps[0].LogicalUnitNumber)
						alreadyMapped = true

						Logc(ctx).WithFields(
							LogFields{
								"lun":    lunPath,
								"igroup": initiatorGroupName,
								"id":     lunID,
							},
						).Debug("LUN already mapped when checking LUN.")
						break
					}
				}
			}
		}
	}
	return alreadyMapped, lunID, nil
}

func (d OntapAPIREST) EnsureLunMapped(
	ctx context.Context, initiatorGroupName, lunPath string, importNotManaged bool,
) (int, error) {
	alreadyMapped, lunID, err := d.isLunMapped(ctx, lunPath, initiatorGroupName, importNotManaged)
	if err != nil {
		return -1, err
	}

	// Map IFF not already mapped
	if !alreadyMapped {
		lunMapResponse, err := d.api.LunMap(ctx, initiatorGroupName, lunPath, lunID)
		if err != nil {
			return -1, fmt.Errorf("err not nil, problem mapping LUN %s: %s", lunPath, err.Error())
		}
		if lunMapResponse == nil {
			return -1, fmt.Errorf("response nil, problem mapping LUN %s: %v", lunPath, err)
		}
		if lunMapResponse.Payload == nil || lunMapResponse.Payload.NumRecords == nil {
			return -1, fmt.Errorf("response payload nil, problem mapping LUN %s: %v", lunPath, err)
		}
		if len(lunMapResponse.Payload.LunMapResponseInlineRecords) > 0 {
			if lunMapResponse.Payload.LunMapResponseInlineRecords[0].LogicalUnitNumber != nil {
				lunID = int(*lunMapResponse.Payload.LunMapResponseInlineRecords[0].LogicalUnitNumber)
			}
		}

		Logc(ctx).WithFields(LogFields{
			"lun":    lunPath,
			"igroup": initiatorGroupName,
			"id":     lunID,
		}).Debug("LUN mapped.")
	}

	return lunID, nil
}

func (d OntapAPIREST) LunUnmap(ctx context.Context, initiatorGroupName, lunPath string) error {
	fields := LogFields{
		"LUN":    lunPath,
		"igroup": initiatorGroupName,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Debug(">>>> LunUnmap.")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< LunUnmap.")

	err := d.api.LunUnmap(ctx, initiatorGroupName, lunPath)
	if err != nil {
		msg := "error unmapping LUN"
		Logc(ctx).WithError(err).Error(msg)
		return fmt.Errorf(msg)
	}
	return nil
}

// LunListIgroupsMapped returns a list of igroups the LUN is currently mapped to.
func (d OntapAPIREST) LunListIgroupsMapped(ctx context.Context, lunPath string) ([]string, error) {
	var names []string

	results, err := d.api.LunMapList(ctx, "*", lunPath)
	if err != nil {
		return names, err
	}
	if results == nil || results.Payload == nil {
		return names, fmt.Errorf("LUN map response is empty")
	}

	for _, records := range results.Payload.LunMapResponseInlineRecords {
		if records.Igroup != nil && records.Igroup.Name != nil {
			names = append(names, *records.Igroup.Name)
		}
	}
	return names, err
}

// IgroupListLUNsMapped returns a list LUNs mapped to the igroup
func (d OntapAPIREST) IgroupListLUNsMapped(ctx context.Context, initiatorGroupName string) ([]string, error) {
	var names []string

	results, err := d.api.LunMapList(ctx, initiatorGroupName, "*")
	if err != nil {
		return names, err
	}
	if results == nil || results.Payload == nil {
		return names, fmt.Errorf("LUN map response is empty")
	}

	for _, records := range results.Payload.LunMapResponseInlineRecords {
		if records.Lun != nil && records.Lun.Name != nil {
			names = append(names, *records.Lun.Name)
		}
	}
	return names, err
}

// LunMapGetReportingNodes returns a list of LUN map details
// equivalent to filer::> lun mapping show -vserver iscsi_vs -path /vol/v/lun0 -igroup trident
func (d OntapAPIREST) LunMapGetReportingNodes(ctx context.Context, initiatorGroupName, lunPath string) (
	[]string, error,
) {
	return d.api.LunMapGetReportingNodes(ctx, initiatorGroupName, lunPath)
}

func (d OntapAPIREST) LunSize(ctx context.Context, flexvolName string) (int, error) {
	lunPath := fmt.Sprintf("/vol/%v/lun0", flexvolName)
	return d.api.LunSize(ctx, lunPath)
}

func (d OntapAPIREST) LunSetSize(ctx context.Context, lunPath, newSize string) (uint64, error) {
	fields := LogFields{
		"Method":  "LunSetSize",
		"Type":    "OntapAPIREST",
		"Name":    lunPath,
		"NewSize": newSize,
	}
	Logd(ctx, d.driverName, d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> LunSetSize")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< LunSetSize")

	return d.api.LunSetSize(ctx, lunPath, newSize)
}

func (d OntapAPIREST) IscsiInitiatorGetDefaultAuth(ctx context.Context) (IscsiInitiatorAuth, error) {
	authInfo := IscsiInitiatorAuth{}
	response, err := d.api.IscsiInitiatorGetDefaultAuth(ctx)
	if err != nil {
		return authInfo, err
	}

	if response == nil || response.Payload == nil {
		return authInfo, fmt.Errorf("iSCSI initiator response is nil")
	}

	if response.Payload.IscsiCredentialsResponseInlineRecords == nil {
		return authInfo, fmt.Errorf("iSCSI initiator response is nil")
	}

	if response.Payload.NumRecords != nil && *response.Payload.NumRecords == 0 {
		return authInfo, fmt.Errorf("iSCSI initiator response has no records")
	}

	if response.Payload.NumRecords != nil && *response.Payload.NumRecords > 1 {
		return authInfo, fmt.Errorf("iSCSI initiator response has too many records")
	}

	record := response.Payload.IscsiCredentialsResponseInlineRecords[0]
	if record.Svm != nil && record.Svm.Name != nil {
		authInfo.SVMName = *record.Svm.Name
	}

	if record.Chap != nil {
		if record.Chap.Inbound != nil {
			if record.Chap.Inbound.User != nil {
				authInfo.ChapUser = *record.Chap.Inbound.User
			}
			if record.Chap.Inbound.Password != nil {
				authInfo.ChapPassphrase = *record.Chap.Inbound.Password
			}
		}

		if record.Chap.Outbound != nil {
			if record.Chap.Outbound.User != nil {
				authInfo.ChapOutboundUser = *record.Chap.Outbound.User
			}
			if record.Chap.Outbound.Password != nil {
				authInfo.ChapOutboundPassphrase = *record.Chap.Outbound.Password
			}
		}
	}

	if record.Initiator != nil {
		authInfo.Initiator = *record.Initiator
	}

	if record.AuthenticationType != nil {
		authInfo.AuthType = *record.AuthenticationType
	}

	return authInfo, nil
}

func (d OntapAPIREST) IscsiInitiatorSetDefaultAuth(
	ctx context.Context, authType, userName, passphrase, outbountUserName, outboundPassphrase string,
) error {
	return d.api.IscsiInitiatorSetDefaultAuth(ctx, authType, userName, passphrase, outbountUserName, outboundPassphrase)
}

func (d OntapAPIREST) IscsiInterfaceGet(ctx context.Context, svm string) ([]string, error) {
	var iSCSINodeNames []string
	interfaceResponse, err := d.api.IscsiInterfaceGet(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not get SVM iSCSI node name: %v", err)
	}
	if interfaceResponse == nil || interfaceResponse.Payload == nil {
		return nil, nil
	}
	// Get the IQN and ensure it is enabled
	for _, record := range interfaceResponse.Payload.IscsiServiceResponseInlineRecords {
		if record.Enabled != nil && *record.Enabled {
			if record.Target != nil && record.Target.Name != nil {
				iSCSINodeNames = append(iSCSINodeNames, *record.Target.Name)
			}
		}
	}

	if len(iSCSINodeNames) == 0 {
		return nil, fmt.Errorf("SVM %s has no active iSCSI interfaces", svm)
	}

	return iSCSINodeNames, nil
}

func (d OntapAPIREST) IscsiNodeGetNameRequest(ctx context.Context) (string, error) {
	result, err := d.api.IscsiNodeGetName(ctx)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", fmt.Errorf("iSCSI node name response is empty")
	}
	if result.Payload == nil {
		return "", fmt.Errorf("iSCSI node name payload is empty")
	}
	if result.Payload.Target == nil {
		return "", fmt.Errorf("could not get iSCSI node name target")
	}
	if result.Payload.Target.Name == nil {
		return "", fmt.Errorf("could not get iSCSI node name target")
	}
	return *result.Payload.Target.Name, nil
}

func (d OntapAPIREST) IgroupCreate(ctx context.Context, initiatorGroupName, initiatorGroupType, osType string) error {
	fields := LogFields{
		"Method":             "IgroupCreate",
		"Type":               "OntapAPIREST",
		"InitiatorGroupName": initiatorGroupName,
		"InitiatorGroupType": initiatorGroupType,
		"OsType":             osType,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> IgroupCreate")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< IgroupCreate")

	igroup, err := d.api.IgroupGetByName(ctx, initiatorGroupName)
	if err != nil {
		return err
	}
	if igroup == nil {
		Logc(ctx).Debugf("igroup %s does not exist, creating new igroup now", initiatorGroupName)
		err = d.api.IgroupCreate(ctx, initiatorGroupName, initiatorGroupType, osType)
		if err != nil {
			if strings.Contains(err.Error(), "[409]") {
				return nil
			}
			return err
		}
	}

	return nil
}

func (d OntapAPIREST) IgroupDestroy(ctx context.Context, initiatorGroupName string) error {
	fields := LogFields{
		"Method":             "IgroupDestroy",
		"Type":               "OntapAPIREST",
		"InitiatorGroupName": initiatorGroupName,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> IgroupDestroy")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< IgroupDestroy")

	err := d.api.IgroupDestroy(ctx, initiatorGroupName)
	if err != nil {
		if !IsNotFoundError(err) {
			Logc(ctx).WithError(err).WithField("igroup", initiatorGroupName).Error("Unable to delete igroup")
			return err
		}
		Logc(ctx).WithError(err).WithField("igroup", initiatorGroupName).Warn("Unable to delete igroup")
	}
	return nil
}

func (d OntapAPIREST) EnsureIgroupAdded(
	ctx context.Context, initiatorGroupName, initiator string,
) error {
	fields := LogFields{
		"Method":             "EnsureIgroupAdded",
		"Type":               "OntapAPIREST",
		"InitiatorGroupName": initiatorGroupName,
		"IQN":                initiator,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> EnsureIgroupAdded")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< EnsureIgroupAdded")

	alreadyAdded, err := d.isIgroupAdded(ctx, initiator, initiatorGroupName)
	if err != nil {
		return err
	}

	if !alreadyAdded {
		Logc(ctx).Debugf("IQN %s not in igroup %s, adding now.", initiator, initiatorGroupName)
		return d.api.IgroupAdd(ctx, initiatorGroupName, initiator)
	}

	return nil
}

func (d OntapAPIREST) isIgroupAdded(ctx context.Context, initiator, initiatorGroupName string) (bool, error) {
	alreadyAdded := false
	igroup, err := d.api.IgroupGetByName(ctx, initiatorGroupName)
	if err != nil {
		return alreadyAdded, err
	}
	if igroup != nil {
		for _, i := range igroup.IgroupInlineInitiators {
			if i.Name != nil && *i.Name == initiator {
				Logc(ctx).Debugf("Initiator %v already in Igroup %v", initiator, initiatorGroupName)
				alreadyAdded = true
				break
			}
		}
	}
	return alreadyAdded, nil
}

func (d OntapAPIREST) IgroupList(ctx context.Context) ([]string, error) {
	fields := LogFields{
		"Method": "IgroupList",
		"Type":   "OntapAPIREST",
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> IgroupList")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< IgroupList")

	igroupsResponse, err := d.api.IgroupList(ctx, "")
	if err != nil {
		return nil, err
	}

	if igroupsResponse.Payload == nil || igroupsResponse.Payload.NumRecords == nil {
		return nil, nil
	}
	igroups := make([]string, 0, *igroupsResponse.Payload.NumRecords)
	for _, igroup := range igroupsResponse.Payload.IgroupResponseInlineRecords {
		if igroup != nil && igroup.Name != nil {
			igroups = append(igroups, *igroup.Name)
		}
	}

	return igroups, nil
}

func (d OntapAPIREST) IgroupRemove(ctx context.Context, initiatorGroupName, initiator string, force bool) error {
	fields := LogFields{
		"Method":             "IgroupRemove",
		"Type":               "OntapAPIREST",
		"InitiatorGroupName": initiatorGroupName,
		"IQN":                initiator,
	}
	Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace(">>>> IgroupRemove")
	defer Logd(ctx, d.driverName,
		d.api.ClientConfig().DebugTraceFlags["method"]).WithFields(fields).Trace("<<<< IgroupRemove")

	return d.api.IgroupRemove(ctx, initiatorGroupName, initiator)
}

func (d OntapAPIREST) IgroupGetByName(ctx context.Context, initiatorGroupName string) (map[string]bool,
	error,
) {
	// Discover mapped initiators
	iGroupResponse, err := d.api.IgroupGetByName(ctx, initiatorGroupName)
	if err != nil {
		return nil, fmt.Errorf("failed to read igroup info; %v", err)
	}
	mappedIQNs := make(map[string]bool)
	if iGroupResponse != nil {
		initiators := iGroupResponse.IgroupInlineInitiators
		for _, initiator := range initiators {
			if initiator.Name != nil {
				mappedIQNs[*initiator.Name] = true
			}
		}
	}
	return mappedIQNs, nil
}

// GetSLMDataLifs returns IP addresses whose node name matches reporting node names
func (d OntapAPIREST) GetSLMDataLifs(ctx context.Context, ips, reportingNodeNames []string) ([]string, error) {
	var reportedDataLIFs []string

	if len(ips) == 0 || len(reportingNodeNames) == 0 {
		return nil, nil
	}

	netInterfaces, err := d.api.NetworkIPInterfacesList(ctx)
	if err != nil {
		return nil, fmt.Errorf("error checking network interfaces: %v", err)
	}

	if netInterfaces == nil || netInterfaces.Payload == nil {
		Logc(ctx).Debug("Net interface REST API returned a empty response.")
		return nil, nil
	}

	if netInterfaces.Payload != nil {
		for _, netInterface := range netInterfaces.Payload.IPInterfaceResponseInlineRecords {
			if netInterface.Location != nil && netInterface.Location.Node != nil && netInterface.IP != nil {
				nodeName := netInterface.Location.Node.Name

				ipAddress := ""
				if netInterface.IP.Address != nil {
					ipAddress = string(*netInterface.IP.Address)
				}

				if nodeName != nil && *nodeName != "" && ipAddress != "" {
					if utils.SliceContainsString(ips, ipAddress) &&
						utils.SliceContainsString(reportingNodeNames, *nodeName) {
						reportedDataLIFs = append(reportedDataLIFs, ipAddress)
					}
				}
			}
		}
	}

	return reportedDataLIFs, nil
}

func (d OntapAPIREST) GetSVMUUID() string {
	return d.api.SVMUUID()
}

func (d OntapAPIREST) SMBShareCreate(ctx context.Context, shareName, path string) error {
	if err := d.api.SMBShareCreate(ctx, shareName, path); err != nil {
		return fmt.Errorf("error while creating SMB share %v : %v", shareName, err)
	}
	return nil
}

func (d OntapAPIREST) SMBShareExists(ctx context.Context, shareName string) (bool, error) {
	share, err := d.api.SMBShareExists(ctx, shareName)
	if err != nil {
		return false, fmt.Errorf("error while checking SMB share %v : %v", shareName, err)
	}
	return share, nil
}

func (d OntapAPIREST) SMBShareDestroy(ctx context.Context, shareName string) error {
	if err := d.api.SMBShareDestroy(ctx, shareName); err != nil {
		return fmt.Errorf("error while deleting SMB share %v: %v", shareName, err)
	}
	return nil
}
