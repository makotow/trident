// Copyright 2016 NetApp, Inc. All Rights Reserved.

package ontap

import (
	"fmt"
	"os/exec"
	"strconv"

	log "github.com/Sirupsen/logrus"
	dvp "github.com/netapp/netappdvp/storage_drivers"

	"github.com/netapp/netappdvp/apis/ontap"
	"github.com/netapp/trident/config"
	"github.com/netapp/trident/storage"
	sa "github.com/netapp/trident/storage_attribute"
)

// OntapSANStorageDriver is for iSCSI storage provisioning
type OntapSANStorageDriver struct {
	dvp.OntapSANStorageDriver
}

// Retrieve storage backend capabilities
func (d *OntapSANStorageDriver) GetStorageBackendSpecs(backend *storage.StorageBackend) error {

	backend.Name = "ontapsan_" + d.Config.DataLIF
	poolAttrs := d.GetStoragePoolAttributes()
	return getStorageBackendSpecsCommon(d, backend, poolAttrs)
}

func (d *OntapSANStorageDriver) GetStoragePoolAttributes() map[string]sa.Offer {

	return map[string]sa.Offer{
		sa.BackendType:      sa.NewStringOffer(d.Name()),
		sa.Snapshots:        sa.NewBoolOffer(true),
		sa.Encryption:       sa.NewBoolOffer(d.API.SupportsApiFeature(ontap.NETAPP_VOLUME_ENCRYPTION)),
		sa.ProvisioningType: sa.NewStringOffer("thick", "thin"),
	}
}

func (d *OntapSANStorageDriver) GetVolumeOpts(
	volConfig *storage.VolumeConfig,
	vc *storage.StoragePool,
	requests map[string]sa.Request,
) (map[string]string, error) {
	return getVolumeOptsCommon(volConfig, vc, requests), nil
}

func (d *OntapSANStorageDriver) GetInternalVolumeName(name string) string {
	return getInternalVolumeNameCommon(d.Config.CommonStorageDriverConfig, name)
}

func (d *OntapSANStorageDriver) CreatePrepare(volConfig *storage.VolumeConfig) bool {
	return createPrepareCommon(d, volConfig)
}

func (d *OntapSANStorageDriver) CreateFollowup(volConfig *storage.VolumeConfig) error {
	return d.mapOntapSANLun(volConfig)
}

func (d *OntapSANStorageDriver) mapOntapSANLun(volConfig *storage.VolumeConfig) error {
	var (
		targetIQN string
		lunID     int
	)

	response, err := d.API.IscsiServiceGetIterRequest()
	if response.Result.ResultStatusAttr != "passed" || err != nil {
		return fmt.Errorf("Problem retrieving iSCSI services: %v, %v",
			err, response.Result.ResultErrnoAttr)
	}
	for _, serviceInfo := range response.Result.AttributesList() {
		if serviceInfo.Vserver() == d.Config.SVM {
			targetIQN = serviceInfo.NodeName()
			log.WithFields(log.Fields{
				"volume":    volConfig.Name,
				"targetIQN": targetIQN,
			}).Debug("Successfully discovered target IQN for the volume.")
			break
		}
	}

	// Map LUN
	lunPath := fmt.Sprintf("/vol/%v/lun0", volConfig.InternalName)
	lunID, err = d.API.LunMapIfNotMapped(d.Config.IgroupName, lunPath)
	if err != nil {
		return err
	}

	volConfig.AccessInfo.IscsiTargetPortal = d.Config.DataLIF
	volConfig.AccessInfo.IscsiTargetIQN = targetIQN
	volConfig.AccessInfo.IscsiLunNumber = int32(lunID)
	volConfig.AccessInfo.IscsiIgroup = d.Config.IgroupName
	log.WithFields(log.Fields{
		"volume":          volConfig.Name,
		"volume_internal": volConfig.InternalName,
		"targetIQN":       volConfig.AccessInfo.IscsiTargetIQN,
		"lunNumber":       volConfig.AccessInfo.IscsiLunNumber,
		"igroup":          volConfig.AccessInfo.IscsiIgroup,
	}).Debug("Successfully mapped ONTAP LUN.")

	return nil
}

func (d *OntapSANStorageDriver) GetProtocol() config.Protocol {
	return config.Block
}

func (d *OntapSANStorageDriver) GetDriverName() string {
	return d.Config.StorageDriverName
}

func (d *OntapSANStorageDriver) StoreConfig(
	b *storage.PersistentStorageBackendConfig,
) {
	storage.SanitizeCommonStorageDriverConfig(
		d.Config.CommonStorageDriverConfig)
	b.OntapConfig = &d.Config
}

func (d *OntapSANStorageDriver) GetExternalConfig() interface{} {
	return getExternalConfig(d.Config)
}

func DiscoverIscsiTarget(targetIP string) error {
	cmd := exec.Command("sudo", "iscsiadm", "-m", "discoverydb", "-t", "st", "-p", targetIP, "--discover")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return err
	} else {
		log.WithFields(log.Fields{
			"target":    targetIP,
			"targetIQN": string(out[:]),
		}).Info("Successful iSCSI target discovery.")
	}
	cmd = exec.Command("sudo", "iscsiadm", "-m", "node", "-p", targetIP, "--login")
	out, err = cmd.CombinedOutput()
	if err != nil {
		return err
	}
	return nil
}

func (d *OntapSANStorageDriver) GetExternalVolume(name string) (*storage.VolumeExternal, error) {

	internalName := d.GetInternalVolumeName(name)
	volumeAttributes, err := d.API.VolumeGet(internalName)
	if err != nil {
		return nil, err
	}
	volumeExportAttrs := volumeAttributes.VolumeExportAttributes()
	volumeIdAttrs := volumeAttributes.VolumeIdAttributes()
	volumeSecurityAttrs := volumeAttributes.VolumeSecurityAttributes()
	volumeSecurityUnixAttributes := volumeSecurityAttrs.VolumeSecurityUnixAttributes()
	volumeSpaceAttrs := volumeAttributes.VolumeSpaceAttributes()
	volumeSnapshotAttrs := volumeAttributes.VolumeSnapshotAttributes()

	volumeConfig := &storage.VolumeConfig{
		Version:         "1",
		Name:            name,
		InternalName:    internalName,
		Size:            string(volumeSpaceAttrs.SizeTotal()),
		Protocol:        config.Block,
		SnapshotPolicy:  volumeSnapshotAttrs.SnapshotPolicy(),
		ExportPolicy:    volumeExportAttrs.Policy(),
		SnapshotDir:     strconv.FormatBool(volumeSnapshotAttrs.SnapdirAccessEnabled()),
		UnixPermissions: volumeSecurityUnixAttributes.Permissions(),
		StorageClass:    "",
		AccessMode:      config.ReadWriteOnce,
		AccessInfo:      storage.VolumeAccessInfo{},
		BlockSize:       "",
		FileSystem:      "ext4",
	}

	volume := &storage.VolumeExternal{
		Config:  volumeConfig,
		Backend: d.Name(),
		Pool:    volumeIdAttrs.ContainingAggregateName(),
	}

	return volume, nil
}
