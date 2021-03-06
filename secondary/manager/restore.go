// Copyright (c) 2014 Couchbase, Inc.

// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package manager

import (
	"fmt"
	"github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/logging"
	"github.com/couchbase/indexing/secondary/planner"
	"unsafe"
)

//////////////////////////////////////////////////////////////
// Concrete Type/Struct
//////////////////////////////////////////////////////////////

type RestoreContext struct {
	clusterUrl   string
	image        *ClusterIndexMetadata
	current      *planner.Plan
	target       string
	idxFromImage map[common.IndexerId][]*planner.IndexUsage
	idxToRestore map[common.IndexerId][]*planner.IndexUsage
	indexerMap   map[common.IndexerId]common.IndexerId
}

//////////////////////////////////////////////////////////////
// RestoreContext
//////////////////////////////////////////////////////////////

//
// Initialize restore context
//
func createRestoreContext(image *ClusterIndexMetadata, clusterUrl string, bucket string) *RestoreContext {

	context := &RestoreContext{
		clusterUrl:   clusterUrl,
		image:        image,
		target:       bucket,
		idxFromImage: make(map[common.IndexerId][]*planner.IndexUsage),
		idxToRestore: make(map[common.IndexerId][]*planner.IndexUsage),
		indexerMap:   make(map[common.IndexerId]common.IndexerId),
	}

	return context
}

//
// Restore and place index in the image onto the current cluster.
//
func (m *RestoreContext) computeIndexLayout() (map[string][]*common.IndexDefn, error) {

	// convert storage mode
	if err := m.convertStorageMode(); err != nil {
		return nil, err
	}

	// convert image to IndexUsage
	if err := m.convertImage(); err != nil {
		return nil, err
	}

	// cleanse the image
	m.cleanseBackupMetadata()

	// Fetch the index layout from current cluster
	current, err := planner.RetrievePlanFromCluster(m.clusterUrl, nil)
	if err != nil {
		return nil, err
	}
	m.current = current

	// find index to restore
	if err := m.findIndexToRestore(); err != nil {
		return nil, err
	}

	// associate indexer from image to current cluster
	m.buildIndexerMapping()

	// build index defnition map
	if err := m.updateIndexDefnId(); err != nil {
		return nil, err
	}

	// invoke placement
	return m.placeIndex()
}

//
// Convert storage mode of index to cluster storage mode
//
func (m *RestoreContext) convertStorageMode() error {

	for i, _ := range m.image.Metadata {
		meta := &m.image.Metadata[i]
		for j, _ := range meta.IndexDefinitions {
			defn := &meta.IndexDefinitions[j]
			defn.Using = "gsi"
		}
	}

	return nil
}

//
// Convert from LocalIndexMetadata to IndexUsage
//
func (m *RestoreContext) convertImage() error {

	config, err := common.GetSettingsConfig(common.SystemConfig)
	if err != nil {
		logging.Errorf("RestoreContext: Error from retrieving indexer settings. Error = %v", err)
		return err
	}

	origBucket := make(map[string]bool)
	for _, meta := range m.image.Metadata {
		for _, defn := range meta.IndexDefinitions {

			// If the index is in CREATED state, this will return nil.  So if there is any create index in flight,
			// it could be excluded by restore.
			indexes, err := planner.ConvertToIndexUsage(config, &defn, (*planner.LocalIndexMetadata)(unsafe.Pointer(&meta)))
			if err != nil {
				return err
			}

			if len(indexes) == 0 {
				logging.Infof("RestoreContext:  Index could be in the process of being created or dropped.  Skip restoring index (%v, %v).",
					defn.Bucket, defn.Name)
				continue
			}

			for _, index := range indexes {

				if index.Instance != nil {
					logging.Infof("RestoreContext:  Processing index in backup image (%v, %v, %v, %v).",
						index.Bucket, index.Name, index.PartnId, index.Instance.ReplicaId)
				} else {
					logging.Infof("RestoreContext:  Processing index in backup image (%v, %v).", index.Bucket, index.Name)
				}

				if len(m.target) != 0 && index.Bucket != m.target {
					logging.Infof("RestoreContext:  convert index %v from bucket %v to target bucket (%v)", index.Name, index.Bucket, m.target)
					origBucket[index.Bucket] = true
					index.Bucket = m.target
					if index.Instance != nil {
						index.Instance.Defn.Bucket = m.target
					}
				}

				m.idxFromImage[common.IndexerId(meta.IndexerId)] = append(m.idxFromImage[common.IndexerId(meta.IndexerId)], index)
			}
		}
	}

	if len(m.target) != 0 && len(origBucket) > 1 {
		return fmt.Errorf("Backup metadata has indexes from multiple buckets.  Cannot restore indexes to a single bucket %v", m.target)
	}

	return nil
}

//
// Remove duplicate index defintion from image.   Dupcliate index defintion include:
// 1) Index with RState=Pending
// 2) Index with different instance versions
//
func (m *RestoreContext) cleanseBackupMetadata() {

	for indexerId, indexes := range m.idxFromImage {
		newIndexes := ([]*planner.IndexUsage)(nil)

		for _, index := range indexes {

			if index.Instance == nil {
				logging.Infof("RestoreContext:  Skip restoring orphan index with no instance metadata (%v, %v, %v).", index.Bucket, index.Name, index.PartnId)
				continue
			}

			// ignore any index with RState being pending.
			// **For pre-spock backup, RState of an instance is ACTIVE (0).
			if index.Instance != nil && index.Instance.RState != common.REBAL_ACTIVE {
				logging.Infof("RestoreContext:  Skip restoring RState PENDING index (%v, %v, %v).", index.Bucket, index.Name, index.PartnId)
				continue
			}

			// find another instance with a higher instance version.
			// **For pre-spock backup, inst version is always 0. In fact, there should not be another instance (max == inst).
			max := findMaxVersionInst(m.idxFromImage, index.DefnId, index.PartnId, index.InstId)
			if max != index {
				logging.Infof("RestoreContext:  Skip restoring index (%v, %v, %v) with lower version number %v.",
					index.Bucket, index.Name, index.PartnId, index.Instance.Version)
				continue
			}

			newIndexes = append(newIndexes, index)
		}

		m.idxFromImage[indexerId] = newIndexes
	}
}

//
// Pick out the indexes that are not yet created in the existing cluster.
//
func (m *RestoreContext) findIndexToRestore() error {

	defnId2NameMap := make(map[common.IndexDefnId]string)
	replicaToRestore := make(map[common.IndexDefnId]map[int]common.IndexInstId)

	for indexerId, indexes := range m.idxFromImage {

		for _, index := range indexes {

			// ignore any index with RState being pending
			// **For pre-spock backup, RState of an instance is ACTIVE (0).
			if index.Instance == nil || index.Instance.RState != common.REBAL_ACTIVE {
				logging.Infof("RestoreContext:  Skip restoring RState PENDING index (%v, %v).", index.Bucket, index.Name)
				continue
			}

			// Find if the index already exist in the current cluster with matching name and bucket.
			anyInst := findMatchingInst(m.current, index.Bucket, index.Name)
			if anyInst != nil {

				// if there is matching index, check if it has the same definition.
				if common.IsEquivalentIndex(&anyInst.Instance.Defn, &index.Instance.Defn) {

					// if it has the same definiton, check if the same replica exist
					// ** For pre-spock backup, ReplicaId is 0
					// ** For pre-vulcan backup, PartnId is 0
					matchingReplica := findMatchingReplica(m.current, index.Bucket, index.Name, index.PartnId, index.Instance.ReplicaId)

					// If same replica exist, then skip.
					if matchingReplica != nil {
						logging.Infof("RestoreContext:  Find index in the target cluster with the same bucket, name, replicaId and definition. "+
							"Skip restoring index (%v, %v, %v, %v).", index.Bucket, index.Name, index.PartnId, index.Instance.ReplicaId)
						continue
					}

					// This replica on the partition does not exit, have we seen the same replica before?
					if _, ok := replicaToRestore[index.DefnId][index.Instance.ReplicaId]; !ok {
						// If same replica does not exist, restore only if the replicaId is smaller than the no. of replica.
						numReplica := findNumReplica(m.current.Placement, anyInst.DefnId) + len(replicaToRestore[index.DefnId])
						if int(anyInst.Instance.Defn.GetNumReplica()+1) <= int(numReplica) {
							logging.Infof("RestoreContext:  Find index in the target cluster with the same bucket, name and definition, but fewer replica. "+
								"Skip restoring index (%v, %v, %v, %v).", index.Bucket, index.Name, index.PartnId, index.Instance.ReplicaId)
							continue
						}
					}

				} else {
					// There is another index with the same name or bucket but different definition.  Re-name the index to restore.
					if _, ok := defnId2NameMap[index.DefnId]; !ok {
						for count := 0; true; count++ {
							newName := fmt.Sprintf("%v_%v", index.Name, count)
							if findMatchingInst(m.current, index.Bucket, newName) == nil {
								defnId2NameMap[index.DefnId] = newName
								break
							}
						}
					}

					logging.Infof("RestoreContext:  Find index (with different defn) in the target cluster with the same bucket and index name .  "+
						" Renaming index from (%v, %v, %v) to (%v, %v, %v).",
						index.Bucket, index.Name, index.Instance.ReplicaId, index.Bucket, defnId2NameMap[index.DefnId], index.Instance.ReplicaId)

					index.Name = defnId2NameMap[index.DefnId]
					index.Instance.Defn.Name = defnId2NameMap[index.DefnId]
				}
			}

			// Find if there is a replica in the existing cluster with the same replicaId (regardless of partition)
			instId := common.IndexInstId(0)
			anyReplica := findAnyReplica(m.current, index.Bucket, index.Name, index.Instance.ReplicaId)
			if anyReplica != nil {
				// There is already a replica with the same replicaId, then use the same InstId
				instId = anyReplica.InstId

			} else {
				// This is a new replica to restore
				if _, ok := replicaToRestore[index.DefnId]; !ok {
					replicaToRestore[index.DefnId] = make(map[int]common.IndexInstId)
				}

				instId = replicaToRestore[index.DefnId][index.Instance.ReplicaId]
				if instId == common.IndexInstId(0) {
					var err error
					instId, err = common.NewIndexInstId()
					if err != nil {
						return err
					}
					replicaToRestore[index.DefnId][index.Instance.ReplicaId] = instId
				}
			}

			temp := *index
			temp.InstId = instId
			if temp.Instance != nil {
				temp.Instance.InstId = instId
			}
			m.idxToRestore[common.IndexerId(indexerId)] = append(m.idxToRestore[common.IndexerId(indexerId)], &temp)
		}
	}

	return nil
}

//
// For each indexer in the image, try to find a correpsonding indexer.
//
func (m *RestoreContext) buildIndexerMapping() {

	// find a match for each indexer node in the image
	excludes := ([]*planner.IndexerNode)(nil)

	for indexerId, indexes := range m.idxFromImage {

		// This will not match any empty indexer node
		if match := findMatchingIndexer(indexerId, indexes, m.current.Placement, excludes); match != nil {

			logging.Infof("RestoreContext:  Map indexer %v in image to indexer %v in current cluster.", indexerId, match.IndexerId)

			m.indexerMap[common.IndexerId(indexerId)] = common.IndexerId(match.IndexerId)
			excludes = append(excludes, match)
		}
	}
}

//
// Update index definition id according to the indexes in current cluster.
//
func (m *RestoreContext) updateIndexDefnId() error {

	defnIdMap := make(map[string]common.IndexDefnId)

	// map indexes in existing cluster
	for _, indexer := range m.current.Placement {
		for _, index := range indexer.Indexes {
			key := fmt.Sprintf("%v %v", index.Bucket, index.Name)
			defnIdMap[key] = index.DefnId
		}
	}

	// map indexes that are to be restored.   Generate a new defnId if
	// the index has not been seen before.
	for _, indexes := range m.idxToRestore {
		for _, index := range indexes {
			key := fmt.Sprintf("%v %v", index.Bucket, index.Name)
			if _, ok := defnIdMap[key]; !ok {
				defnId, err := common.NewIndexDefnId()
				if err != nil {
					logging.Errorf("RestoreContext: fail to generate index definition id %v", err)
					return err
				}
				defnIdMap[key] = defnId
			}

			// Planner depends on 3 fields
			// 1) Index Defnition Id
			// 2) ReplicaId
			// 3) PartitionId
			// Regenerate IndexDefnId for all replica/partitions with the same <bucket,name>
			index.DefnId = defnIdMap[key]
			if index.Instance != nil {
				index.Instance.Defn.DefnId = defnIdMap[key]
			}
		}
	}

	return nil
}

//
// Place index
//
func (m *RestoreContext) placeIndex() (map[string][]*common.IndexDefn, error) {

	newNodes := ([]*planner.IndexerNode)(nil)
	newNodeIds := ([]string)(nil)
	mappedIndexers := ([]*planner.IndexerNode)(nil)

	// Go through the index that needs to be restored
	for indexerId, indexes := range m.idxToRestore {

		// Found a matching indexer in the target cluster
		if targetIndexerId, ok := m.indexerMap[indexerId]; ok {
			if indexer := findIndexer(m.current, targetIndexerId); indexer != nil {
				addIndexes(indexes, indexer)
				mappedIndexers = append(mappedIndexers, indexer)
				continue
			}
		}

		// cannot find a matching indexer in the target.  Add a new node
		// to the current plan
		indexer := createNewEjectedNode(indexes, indexerId)
		newNodes = append(newNodes, indexer)
		newNodeIds = append(newNodeIds, indexer.NodeId)
	}

	// add new nodes to current plan
	for _, indexer := range newNodes {
		m.current.Placement = append(m.current.Placement, indexer)
	}

	logging.Infof("RestoreContext:  Index Layout before planning")
	for _, indexer := range m.current.Placement {
		logging.Infof("\tRestoreContext:  Indexer NodeId %v IndexerId %v", indexer.NodeId, indexer.IndexerId)
		for _, index := range indexer.Indexes {
			if index.Instance != nil {
				logging.Infof("\t\tRestoreContext:  Index (%v, %v, %v)", index.Bucket, index.Name, index.Instance.ReplicaId)
			} else {
				logging.Infof("\t\tRestoreContext:  Index (%v, %v)", index.Bucket, index.Name)
			}
		}
	}

	// If there is enough empty nodes in the current clsuter to do a simple swap rebalance.
	numEmptyIndexer := findNumEmptyIndexer(m.current.Placement, mappedIndexers)
	if numEmptyIndexer >= len(newNodes) {
		// place indexes using swap rebalance
		solution, err := planner.ExecuteSwapWithOptions(m.current, true, "", "", 0, -1, -1, false, newNodeIds)
		if err == nil {
			return m.buildIndexHostMapping(solution), nil
		}
	}

	// place indexes using regular rebalance
	solution, err := planner.ExecuteRebalanceWithOptions(m.current, nil, true, "", "", 0, -1, -1, false, newNodeIds)
	if err == nil {
		return m.buildIndexHostMapping(solution), nil
	}

	if err != nil {
		logging.Errorf("RestoreContext:  Fail to plan index during restore.  Error = %v", err)
	}

	return nil, err
}

//
// Find new home for the index
//
func (m *RestoreContext) buildIndexHostMapping(solution *planner.Solution) map[string][]*common.IndexDefn {

	result := make(map[string][]*common.IndexDefn)

	for _, indexes := range m.idxToRestore {
		for _, index := range indexes {
			if index.Instance != nil {
				if indexer := solution.FindIndexerWithReplica(index.Name, index.Bucket, index.PartnId, index.Instance.ReplicaId); indexer != nil {
					logging.Infof("RestoreContext:  Restoring index (%v, %v, %v, %v) at indexer %v",
						index.Bucket, index.Name, index.PartnId, index.Instance.ReplicaId, indexer.NodeId)

					defns := result[indexer.RestUrl]
					found := false
					for _, defn := range defns {
						if defn.DefnId == index.Instance.Defn.DefnId && defn.ReplicaId == index.Instance.ReplicaId {
							found = true
							defn.Partitions = append(defn.Partitions, index.PartnId)
							defn.Versions = append(defn.Versions, 0)
							break
						}
					}

					if !found {
						if index.Instance != nil {
							// Reset bucketUUID since it would have changed
							index.Instance.Defn.BucketUUID = ""

							// update transient fields
							index.Instance.Defn.InstId = index.Instance.InstId
							index.Instance.Defn.ReplicaId = index.Instance.ReplicaId
							index.Instance.Defn.Partitions = []common.PartitionId{index.PartnId}
							index.Instance.Defn.Versions = []int{0}
							index.Instance.Defn.InstVersion = 0
							index.Instance.Defn.NumPartitions = uint32(index.Instance.Pc.GetNumPartitions())

							result[indexer.RestUrl] = append(result[indexer.RestUrl], &index.Instance.Defn)
						} else {
							logging.Errorf("RestoreContext:  Cannot sestoring index (%v, %v, %v, %v) at indexer %v because index instance is missing.",
								index.Bucket, index.Name, index.PartnId, index.Instance.ReplicaId, indexer.NodeId)
						}
					}
				}
			}
		}
	}

	return result
}

//////////////////////////////////////////////////////////////
// Utility
//////////////////////////////////////////////////////////////

//
// Find a higest version index instance with the same definition id and instance id
//
func findMaxVersionInst(metadata map[common.IndexerId][]*planner.IndexUsage, defnId common.IndexDefnId, partnId common.PartitionId,
	instId common.IndexInstId) *planner.IndexUsage {

	max := (*planner.IndexUsage)(nil)

	for _, indexes := range metadata {
		for _, index := range indexes {

			// ignore any index with RState being pending
			if index.Instance == nil || index.Instance.RState != common.REBAL_ACTIVE {
				continue
			}

			if index.DefnId == defnId && index.PartnId == partnId && index.InstId == instId {
				if max == nil || index.Instance.Version > max.Instance.Version {
					max = index
				}
			}
		}
	}

	return max
}

//
// Find any instance in the metadata, regardless of its partitionId, version or RState
//
func findMatchingInst(current *planner.Plan, bucket string, name string) *planner.IndexUsage {

	for _, indexers := range current.Placement {
		for _, index := range indexers.Indexes {
			if index.Instance != nil {
				if index.Bucket == bucket && index.Name == name {
					return index
				}
			}
		}
	}

	return nil
}

//
// Find matching replica in the metadata, regardless of its version or RState
//
func findMatchingReplica(current *planner.Plan, bucket string, name string, partnId common.PartitionId, replicaId int) *planner.IndexUsage {

	for _, indexers := range current.Placement {
		for _, index := range indexers.Indexes {
			if index.Instance != nil {
				if index.Bucket == bucket && index.Name == name && index.PartnId == partnId && index.Instance.ReplicaId == replicaId {
					return index
				}
			}
		}
	}

	return nil
}

//
// Find any replica in the metadata, regardless of its partitionId, version or RState
//
func findAnyReplica(current *planner.Plan, bucket string, name string, replicaId int) *planner.IndexUsage {

	for _, indexers := range current.Placement {
		for _, index := range indexers.Indexes {
			if index.Instance != nil {
				if index.Bucket == bucket && index.Name == name && index.Instance.ReplicaId == replicaId {
					return index
				}
			}
		}
	}

	return nil
}

//
// Find a node in candidates which is a subset of the given indexes.  This is to honor
// original layout even if the restore operation has to restart in a cluster that is
// partially restored.
//
func findMatchingIndexer(indexerId common.IndexerId, indexes []*planner.IndexUsage,
	candidates []*planner.IndexerNode, excludes []*planner.IndexerNode) *planner.IndexerNode {

	// nothing to match
	if len(indexes) == 0 {
		return nil
	}

	for _, indexer := range candidates {

		// should we consider this target?
		if isMember(excludes, indexer) {
			continue
		}

		// If indexerId match
		if indexerId == common.IndexerId(indexer.IndexerId) {
			return indexer
		}

		// is target a subset of source?
		if len(indexer.Indexes) != 0 && isSubset(indexes, indexer.Indexes) {
			return indexer
		}
	}

	return nil
}

//
// Find the number of empty indexer nodes
//
func findNumEmptyIndexer(indexers []*planner.IndexerNode, excludes []*planner.IndexerNode) int {

	count := 0
	for _, indexer := range indexers {

		// should we consider this target?
		if isMember(excludes, indexer) {
			continue
		}

		if len(indexer.Indexes) == 0 {
			count++
		}
	}

	return count
}

//
// Find if there is a match in the candidate list
//
func isMember(candidates []*planner.IndexerNode, indexer *planner.IndexerNode) bool {

	for _, candidate := range candidates {
		if candidate == indexer {
			return true
		}
	}

	return false
}

//
// Is one a subset of another?
//
func isSubset(superset []*planner.IndexUsage, subset []*planner.IndexUsage) bool {

	for _, sub := range subset {
		found := false
		for _, super := range superset {
			if super.Bucket == sub.Bucket &&
				super.Name == sub.Name &&
				super.PartnId == sub.PartnId &&
				super.Instance != nil && sub.Instance != nil && super.Instance.ReplicaId == sub.Instance.ReplicaId {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

//
// Find the indexer node given the indexerId
//
func findIndexer(plan *planner.Plan, indexerId common.IndexerId) *planner.IndexerNode {

	for _, indexer := range plan.Placement {
		if common.IndexerId(indexer.IndexerId) == indexerId {
			return indexer
		}
	}

	return nil

}

//
// Create a new "ejected" indexer node to hold the indexes to restore
//
func createNewEjectedNode(indexes []*planner.IndexUsage, indexerId common.IndexerId) *planner.IndexerNode {

	nodeId := fmt.Sprintf("%v", indexerId)
	node := planner.CreateIndexerNodeWithIndexes(nodeId, nil, indexes)
	node.IndexerId = string(indexerId)
	node.MarkDeleted()

	return node
}

//
// Add Indexes to the indexer node
//
func addIndexes(indexes []*planner.IndexUsage, indexer *planner.IndexerNode) {

	for _, index := range indexes {
		indexer.Indexes = append(indexer.Indexes, index)
	}
}

//
// Find the number of replica across partitions for an index (including itself).
//
func findNumReplica(indexers []*planner.IndexerNode, defnId common.IndexDefnId) int {

	replicaMap := make(map[int]bool)
	for _, indexer := range indexers {
		for _, index := range indexer.Indexes {
			if defnId == index.DefnId {
				replicaMap[index.Instance.ReplicaId] = true
			}
		}
	}

	return len(replicaMap)
}
