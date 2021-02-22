package shell

import (
	"flag"
	"fmt"
	"github.com/chrislusf/seaweedfs/weed/pb/master_pb"
	"github.com/chrislusf/seaweedfs/weed/storage/types"
	"io"
	"time"

	"github.com/chrislusf/seaweedfs/weed/storage/needle"
)

func init() {
	Commands = append(Commands, &commandVolumeTierMove{})
}

type commandVolumeTierMove struct {
}

func (c *commandVolumeTierMove) Name() string {
	return "volume.tier.move"
}

func (c *commandVolumeTierMove) Help() string {
	return `change a volume from one disk type to another

	volume.tier.move -fromDiskType=hdd -toDiskType=ssd [-collection=""] [-fullPercent=95] [-quietFor=1h]

	Even if the volume is replicated, only one replica will be changed and the rest replicas will be dropped.
	So "volume.fix.replication" and "volume.balance" should be followed.

`
}

func (c *commandVolumeTierMove) Do(args []string, commandEnv *CommandEnv, writer io.Writer) (err error) {

	if err = commandEnv.confirmIsLocked(); err != nil {
		return
	}

	tierCommand := flag.NewFlagSet(c.Name(), flag.ContinueOnError)
	collection := tierCommand.String("collection", "", "the collection name")
	fullPercentage := tierCommand.Float64("fullPercent", 95, "the volume reaches the percentage of max volume size")
	quietPeriod := tierCommand.Duration("quietFor", 24*time.Hour, "select volumes without no writes for this period")
	source := tierCommand.String("fromDiskType", "", "the source disk type")
	target := tierCommand.String("toDiskType", "", "the target disk type")
	applyChange := tierCommand.Bool("force", false, "actually apply the changes")
	if err = tierCommand.Parse(args); err != nil {
		return nil
	}

	fromDiskType := types.ToDiskType(*source)
	toDiskType := types.ToDiskType(*target)

	if fromDiskType == toDiskType {
		return fmt.Errorf("source tier %s is the same as target tier %s", fromDiskType, toDiskType)
	}

	// collect topology information
	topologyInfo, volumeSizeLimitMb, err := collectTopologyInfo(commandEnv)
	if err != nil {
		return err
	}

	// collect all volumes that should change
	volumeIds, err := collectVolumeIdsForTierChange(commandEnv, topologyInfo, volumeSizeLimitMb, fromDiskType, *collection, *fullPercentage, *quietPeriod)
	if err != nil {
		return err
	}
	fmt.Printf("tier move volumes: %v\n", volumeIds)

	_, allLocations := collectVolumeReplicaLocations(topologyInfo)
	for _, vid := range volumeIds {
		if err = doVolumeTierMove(commandEnv, writer, *collection, vid, toDiskType, allLocations, *applyChange); err != nil {
			fmt.Printf("tier move volume %d: %v\n", vid, err)
		}
	}

	return nil
}

func doVolumeTierMove(commandEnv *CommandEnv, writer io.Writer, collection string, vid needle.VolumeId, toDiskType types.DiskType, allLocations []location, applyChanges bool) (err error) {
	// find volume location
	locations, found := commandEnv.MasterClient.GetLocations(uint32(vid))
	if !found {
		return fmt.Errorf("volume %d not found", vid)
	}

	// find one server with the most empty volume slots with target disk type
	hasFoundTarget := false
	keepDataNodesSorted(allLocations, toDiskType)
	fn := capacityByFreeVolumeCount(toDiskType)
	for _, dst := range allLocations {
		if fn(dst.dataNode) > 0 {
			// ask the volume server to replicate the volume
			sourceVolumeServer := ""
			for _, loc := range locations {
				if loc.Url != dst.dataNode.Id {
					sourceVolumeServer = loc.Url
				}
			}
			if sourceVolumeServer == "" {
				continue
			}
			fmt.Fprintf(writer, "moving volume %d %s from %s to dataNode %s with disk type ...\n", vid, sourceVolumeServer, dst.dataNode.Id, toDiskType.String())
			hasFoundTarget = true

			if !applyChanges {
				break
			}

			// mark all replicas as read only
			err = markVolumeReadonly(commandEnv.option.GrpcDialOption, vid, locations)
			if err != nil {
				return fmt.Errorf("mark volume %d as readonly on %s: %v", vid, locations[0].Url, err)
			}
			return LiveMoveVolume(commandEnv.option.GrpcDialOption, vid, sourceVolumeServer, dst.dataNode.Id, 5*time.Second, toDiskType.String())

		}
	}

	if !hasFoundTarget {
		fmt.Fprintf(writer, "can not find disk type %s for volume %d\n", toDiskType.String(), vid)
	}

	return nil
}

func collectVolumeIdsForTierChange(commandEnv *CommandEnv, topologyInfo *master_pb.TopologyInfo, volumeSizeLimitMb uint64, sourceTier types.DiskType, selectedCollection string, fullPercentage float64, quietPeriod time.Duration) (vids []needle.VolumeId, err error) {

	quietSeconds := int64(quietPeriod / time.Second)
	nowUnixSeconds := time.Now().Unix()

	fmt.Printf("collect %s volumes quiet for: %d seconds\n", sourceTier, quietSeconds)

	vidMap := make(map[uint32]bool)
	eachDataNode(topologyInfo, func(dc string, rack RackId, dn *master_pb.DataNodeInfo) {
		for _, diskInfo := range dn.DiskInfos {
			for _, v := range diskInfo.VolumeInfos {
				if v.Collection == selectedCollection && v.ModifiedAtSecond+quietSeconds < nowUnixSeconds && types.ToDiskType(v.DiskType) == sourceTier {
					if float64(v.Size) > fullPercentage/100*float64(volumeSizeLimitMb)*1024*1024 {
						vidMap[v.Id] = true
					}
				}
			}
		}
	})

	for vid := range vidMap {
		vids = append(vids, needle.VolumeId(vid))
	}

	return
}
