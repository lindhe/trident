// Copyright 2022 NetApp, Inc. All Rights Reserved.

package csi

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strings"

	"github.com/spf13/afero"

	"github.com/netapp/trident/config"
	. "github.com/netapp/trident/logging"
	"github.com/netapp/trident/utils"
	"github.com/netapp/trident/utils/errors"
)

const volumePublishInfoFilename = "volumePublishInfo.json"

var (
	osFs        = afero.NewOsFs()
	fileDeleter = utils.DeleteFile
)

type VolumePublishManager struct {
	volumeTrackingInfoPath string
}

type fileDeleterType func(context.Context, string, string) (string, error)

func NewVolumePublishManager(
	volumeTrackingInfoPath string,
) *VolumePublishManager {
	volManager := &VolumePublishManager{
		volumeTrackingInfoPath: volumeTrackingInfoPath,
	}
	return volManager
}

// GetVolumeTrackingFiles returns all the tracking files in the tracking directory.
func (v *VolumePublishManager) GetVolumeTrackingFiles() ([]os.FileInfo, error) {
	return afero.ReadDir(osFs, v.volumeTrackingInfoPath)
}

// WriteTrackingInfo writes the serialized staging target path, publish info and publish paths for a volume to
// Trident's own volume tracking location (/var/lib/trident/tracking) so that node bookkeeping is possible for
// features like force detach
func (v *VolumePublishManager) WriteTrackingInfo(
	ctx context.Context, volumeID string, trackingInfo *utils.VolumeTrackingInfo,
) error {
	fields := LogFields{"volumeID": volumeID}
	Logc(ctx).WithFields(fields).Trace(">>>> WriteTrackingInfo")
	defer Logc(ctx).WithFields(fields).Trace("<<<< WriteTrackingInfo")

	filename := volumeID + ".json"
	tmpFile := "tmp-" + filename

	// Write to a tmp tmpFile first just in case the node goes down or the write otherwise fails, then move to the final
	// destination once it succeeds (a rename is just moving a filesystem pointer, not additional I/O).
	tmpFile = path.Join(v.volumeTrackingInfoPath, tmpFile)
	Logc(ctx).WithField("tempFile", tmpFile).Debug("Writing temporary tracking info file.")
	err := utils.JsonReaderWriter.WriteJSONFile(ctx, trackingInfo, tmpFile, "volume tracking info")
	if err != nil {
		return err
	}

	trackingFile := path.Join(v.volumeTrackingInfoPath, filename)

	Logc(ctx).WithField("fileName", trackingFile).Debug("Updating tracking info file.")
	return osFs.Rename(tmpFile, trackingFile)
}

// ReadTrackingInfo reads from Trident's own volume tracking location (/var/lib/trident/tracking)
// and returns the staging target path, publish info and publish paths for a volume
func (v *VolumePublishManager) ReadTrackingInfo(
	ctx context.Context, volumeID string,
) (*utils.VolumeTrackingInfo, error) {
	fields := LogFields{"volumeID": volumeID}
	Logc(ctx).WithFields(fields).Trace(">>>> ReadTrackingInfo")
	defer Logc(ctx).WithFields(fields).Trace("<<<< ReadTrackingInfo")

	return v.readTrackingInfo(ctx, volumeID)
}

func (v *VolumePublishManager) readTrackingInfo(
	ctx context.Context, volumeID string,
) (*utils.VolumeTrackingInfo, error) {
	var volumeTrackingInfo utils.VolumeTrackingInfo
	filename := volumeID + ".json"
	err := utils.JsonReaderWriter.ReadJSONFile(ctx, &volumeTrackingInfo, path.Join(v.volumeTrackingInfoPath, filename),
		"volume tracking info")
	if err != nil {
		return nil, err
	}

	Logc(ctx).WithField("volumeTrackingInfo", volumeTrackingInfo).Debug("Volume tracking info found.")
	return &volumeTrackingInfo, nil
}

// ListVolumeTrackingInfo returns a map of tracking files to their contents (volume tracking information).
func (v *VolumePublishManager) ListVolumeTrackingInfo(ctx context.Context) (map[string]*utils.VolumeTrackingInfo, error) {
	Logc(ctx).Debug(">>>> ListVolumeTrackingInfo")
	defer Logc(ctx).Debug("<<<< ListVolumeTrackingInfo")

	// Volumes have a 1-1 relationship with a tracking file. A tracking file may contain 1-many published paths.
	files, err := v.GetVolumeTrackingFiles()
	if err != nil {
		return nil, fmt.Errorf("could not find volume tracking info files; %s", err)
	}

	if len(files) == 0 {
		Logc(ctx).Debug("No tracking files found.")
		return nil, errors.NotFoundError("no tracking files found")
	}

	// Discover the tracking files and their volume tracking info.
	trackingFiles := make(map[string]*utils.VolumeTrackingInfo, len(files))
	for _, file := range files {
		volumeID := strings.ReplaceAll(file.Name(), ".json", "")
		trackingInfo, err := v.readTrackingInfo(ctx, volumeID)
		if err != nil {
			return nil, err
		}
		trackingFiles[volumeID] = trackingInfo
	}

	return trackingFiles, nil
}

// DeleteTrackingInfo deletes the tracking info staging target path info for a volume from Trident's own
// volume tracking location (/var/lib/trident/tracking). This method is idempotent, in that if the file doesn't exist,
// no error is generated.
func (v *VolumePublishManager) DeleteTrackingInfo(ctx context.Context, volumeID string) error {
	fields := LogFields{"volumeID": volumeID}
	Logc(ctx).WithFields(fields).Trace(">>>> DeleteTrackingInfo")
	defer Logc(ctx).WithFields(fields).Trace("<<<< DeleteTrackingInfo")

	filename, err := fileDeleter(ctx, path.Join(v.volumeTrackingInfoPath, volumeID+".json"), "tracking")
	if err != nil {
		return err
	}

	Logc(ctx).WithField("trackingFilename", filename).Debug("Removed tracking file.")
	return nil
}

// UpgradeVolumeTrackingFile ensures the published paths for the volume will be discovered (because we
// previously did not track them) and then an attempt to upgrade the volume tracking file will be made. If either the
// stagedDeviceInfo or legacy tracking file do not exist, or are unable to be unmarshalled, then an upgrade is not
// possible, and we must delete the tracking file because it no longer has any value.
func (v *VolumePublishManager) UpgradeVolumeTrackingFile(
	ctx context.Context, volumeId string, publishedPaths map[string]struct{},
) (bool, error) {
	var err error
	fields := LogFields{"volumeId": volumeId}
	Logc(ctx).WithFields(fields).Trace(">>>> UpgradeVolumeTrackingFile")
	defer Logc(ctx).WithFields(fields).Trace("<<<< UpgradeVolumeTrackingFile")

	volumeTrackingInfo := &utils.VolumeTrackingInfo{}
	publishInfo := &utils.VolumePublishInfo{}

	errorTemplate := "error upgrading the volume tracking file for volume: %s :%v"

	file := path.Join(v.volumeTrackingInfoPath, volumeId+".json")
	err = utils.JsonReaderWriter.ReadJSONFile(ctx, volumeTrackingInfo, file, "volume tracking info")
	if err != nil {
		if !isFileValidJSON(err) {
			return true, nil
		}
		return false, TerminalReconciliationError(fmt.Sprintf(errorTemplate, volumeId, err))
	}

	// If the tracking file is the old kind, the filesystem type will be set to the zero value for the string type
	// upon unmarshalling the json.
	if volumeTrackingInfo.VolumePublishInfo.FilesystemType != "" {
		Logc(ctx).Debug("Volume tracking method did not need to be upgraded.")
		return false, nil
	}
	file = path.Join(volumeTrackingInfo.StagingTargetPath, volumePublishInfoFilename)
	err = utils.JsonReaderWriter.ReadJSONFile(ctx, publishInfo, file, "publish info")
	if err != nil {
		if !isFileValidJSON(err) {
			// If the staged device info file is not valid, it will never be useful again, regardless of retries.
			deleteStagedDeviceInfo(ctx, volumeTrackingInfo.StagingTargetPath, volumeId)
			return true, nil
		}
		return false, TerminalReconciliationError(fmt.Sprintf(errorTemplate, volumeId, err))
	}

	_, err = getVolumeProtocolFromPublishInfo(publishInfo)
	if err != nil {
		// If we cannot determine the volume protocol from the staged device info, then there is no reason to keep
		// it around.
		deleteStagedDeviceInfo(ctx, volumeTrackingInfo.StagingTargetPath, volumeId)
		return true, nil
	}

	volumeTrackingInfo.VolumePublishInfo = *publishInfo
	volumeTrackingInfo.PublishedPaths = publishedPaths

	Logc(ctx).WithField("publishInfoLocation", volumeTrackingInfo).Debug("Publish info location found.")
	err = v.WriteTrackingInfo(ctx, volumeId, volumeTrackingInfo)
	if err != nil {
		return false, TerminalReconciliationError(fmt.Sprintf(errorTemplate, volumeId, err))
	}

	// Remove the old file in the staging path now that its contents have been moved to the new tracking file.
	_ = clearStagedDeviceInfo(ctx, volumeTrackingInfo.StagingTargetPath, volumeId)

	Logc(ctx).Debug("Volume tracking method upgraded.")

	return false, nil
}

// ValidateTrackingFile checks whether a tracking file needs to be deleted.
func (v *VolumePublishManager) ValidateTrackingFile(ctx context.Context, volumeId string) (bool, error) {
	var trackingInfo utils.VolumeTrackingInfo
	fields := LogFields{"volumeId": volumeId}
	Logc(ctx).WithFields(fields).Trace(">>>> ValidateTrackingFile")
	defer Logc(ctx).WithFields(fields).Trace("<<<< ValidateTrackingFile")
	filename := path.Join(v.volumeTrackingInfoPath, volumeId+".json")

	err := utils.JsonReaderWriter.ReadJSONFile(ctx, &trackingInfo, filename, "volume tracking")
	if err != nil {
		if !isFileValidJSON(err) {
			return true, nil
		}
		return false, TerminalReconciliationError(err.Error())
	}

	stagePath := trackingInfo.StagingTargetPath
	// The value of the stagingTargetPath in Windows tracking files that were upgraded is incorrect, so there is no
	// value in checking the directory. Therefore, we return early before doing so. False is returned here because
	// in SMB, the stagingTargetPath is the volume attachment point (a symlink), and our only way of checking whether
	// the volume still exists. Since we can't check if the volume attachment point exists, it is not safe to delete the
	// tracking file.
	if v.isUpgradedWindowsTrackingFile(&trackingInfo) {
		return false, nil
	}

	_, err = osFs.Stat(stagePath)
	if err != nil {
		// If the stat failed for any reason other than it not existing, we need to return a failed validation error so
		// that the node plugin will be restarted, and validation can be retried.
		if !errors.Is(err, fs.ErrNotExist) {
			return false, TerminalReconciliationError(err.Error())
		}
		// If the staging path for the volume doesn't exist, that does not automatically mean that the volume is gone.
		// Therefore, we have to check protocol specific conditions to be sure.
		debugMsg := fmt.Sprintf("Staging path %s for volume %s did not exist.", stagePath, volumeId)
		Logc(ctx).Debug(debugMsg)
		volumeMayExist, err := performProtocolSpecificReconciliation(ctx, &trackingInfo)
		if err != nil {
			if IsInvalidTrackingFileError(err) {
				return true, nil
			}
			return false, TerminalReconciliationError(err.Error())
		}
		if !volumeMayExist {
			return true, nil
		}
	}

	return false, nil
}

// DeleteFailedUpgradeTrackingFile deletes all tracking files whose writes were interrupted and caused a failure before
// the atomic write (os.Rename) happened. Deleting these failed upgrade files will not lose information, as the original
// files are left in place on error. Therefore, upgrades will be retried on the next reconcile loop.
func (v *VolumePublishManager) DeleteFailedUpgradeTrackingFile(ctx context.Context, file os.FileInfo) {
	filename := path.Join(config.VolumeTrackingInfoPath, file.Name())

	if strings.Contains(file.Name(), "tmp") {
		_, err := fileDeleter(ctx, filename, "tmp volume tracking file")
		if err != nil {
			Logc(ctx).WithField("filename", filename).Warn("Could not delete temporary volume tracking file.")
		}
	}
}

// clearStagedDeviceInfo removes the volume info at the staging target path.  This method is idempotent,
// in that if the file doesn't exist, no error is generated.
func clearStagedDeviceInfo(ctx context.Context, stagingTargetPath, volumeId string) error {
	fields := LogFields{"stagingTargetPath": stagingTargetPath, "volumeId": volumeId}
	Logc(ctx).WithFields(fields).Trace(">>>> clearStagedDeviceInfo")
	defer Logc(ctx).WithFields(fields).Trace("<<<< clearStagedDeviceInfo")

	stagingFilename := path.Join(stagingTargetPath, volumePublishInfoFilename)

	if err := osFs.Remove(stagingFilename); err != nil {
		if os.IsNotExist(err) {
			Logc(ctx).WithFields(fields).Warning("Staging file does not exist.")
			return nil
		}

		Logc(ctx).WithFields(fields).WithError(err).Error("Removing staging file failed.")
		return err
	}

	Logc(ctx).WithField("stagingFilename", stagingFilename).Debug("Removed staging file.")
	return nil
}

func isFileValidJSON(err error) bool {
	if errors.IsInvalidJSONError(err) || errors.IsNotFoundError(err) {
		return false
	}

	return true
}

func deleteStagedDeviceInfo(ctx context.Context, stagingPath, volumeId string) {
	err := clearStagedDeviceInfo(ctx, stagingPath, volumeId)
	if err != nil {
		fields := LogFields{"volumeId": volumeId, "stagingPath": stagingPath}
		Logc(ctx).WithFields(fields).Warning(fmt.Sprintf("Error deleting staged device info: %v", err))
	}
}

// Previous versions of Trident stored the path where the volumePublishInfo.json was stored for SMB volumes, instead
// of the stagingTargetPath, which is the attachment point for an SMB volume. We need to know if the "stagingTargetPath"
// in the tracking file is the actual stagingTargetPath or not.
func (v *VolumePublishManager) isUpgradedWindowsTrackingFile(trackInfo *utils.VolumeTrackingInfo) bool {
	return runtime.GOOS == "windows" && strings.Contains(trackInfo.StagingTargetPath, v.volumeTrackingInfoPath)
}
