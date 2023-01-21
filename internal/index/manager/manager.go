package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/exp/slices"
	"golang.org/x/sys/unix"

	"github.com/fsnotify/fsnotify"
	"github.com/spq/pkappa2/internal/index"
	"github.com/spq/pkappa2/internal/index/builder"
	"github.com/spq/pkappa2/internal/index/converters"
	"github.com/spq/pkappa2/internal/query"
	"github.com/spq/pkappa2/internal/tools"
	"github.com/spq/pkappa2/internal/tools/bitmask"
	pcapmetadata "github.com/spq/pkappa2/internal/tools/pcapMetadata"
)

type (
	tag struct {
		query.TagDetails
		definition string
		features   query.FeatureSet
		color      string
		converters []*converters.CachedConverter // TODO: put this into TagDetails too?
	}
	TagInfo struct {
		Name           string
		Definition     string
		Color          string
		MatchingCount  uint
		UncertainCount uint
		Referenced     bool
		Converters     []string
	}
	Manager struct {
		StateDir     string
		PcapDir      string
		IndexDir     string
		SnapshotDir  string
		ConverterDir string

		jobs                chan func()
		mergeJobRunning     bool
		taggingJobRunning   bool
		converterJobRunning bool
		importJobs          []string

		builder             *builder.Builder
		indexes             []*index.Reader
		nStreams, nPackets  int
		nextStreamID        uint64
		nUnmergeableIndexes int
		stateFilename       string
		allStreams          bitmask.LongBitmask

		updatedStreamsDuringTaggingJob bitmask.LongBitmask
		addedStreamsDuringTaggingJob   bitmask.LongBitmask

		streamsToConvert map[string]*bitmask.LongBitmask

		tags       map[string]*tag
		converters map[string]*converters.CachedConverter

		usedIndexes map[*index.Reader]uint
		watcher     *fsnotify.Watcher
	}

	Statistics struct {
		ImportJobCount      int
		IndexCount          int
		IndexLockCount      uint
		PcapCount           int
		StreamCount         int
		PacketCount         int
		MergeJobRunning     bool
		TaggingJobRunning   bool
		ConverterJobRunning bool
	}

	indexReleaser []*index.Reader

	// TODO: Maybe save md5 of converters to detect changes
	stateFile struct {
		Saved time.Time
		Tags  []struct {
			Name       string
			Definition string
			Color      string
			Converters []string
		}
		Pcaps []*pcapmetadata.PcapInfo
	}

	updateTagOperationInfo struct {
		markTagAddStreams, markTagDelStreams []uint64
		color                                string
		setConverterNames                    []string
		convertersUpdated                    bool
	}
	UpdateTagOperation func(*updateTagOperationInfo)

	View struct {
		mgr *Manager

		indexes  []*index.Reader
		releaser indexReleaser

		tagDetails map[string]query.TagDetails
		converters map[string]index.ConverterAccess
	}

	StreamContext struct {
		s *index.Stream
		v *View
	}

	streamsOptions struct {
		prefetchTags       []string
		defaultLimit, page uint
		prefetchAllTags    bool
	}
	StreamsOption func(*streamsOptions)
)

func New(pcapDir, indexDir, snapshotDir, stateDir, converterDir string) (*Manager, error) {
	mgr := Manager{
		PcapDir:      pcapDir,
		IndexDir:     indexDir,
		SnapshotDir:  snapshotDir,
		StateDir:     stateDir,
		ConverterDir: converterDir,

		usedIndexes:      make(map[*index.Reader]uint),
		tags:             make(map[string]*tag),
		converters:       make(map[string]*converters.CachedConverter),
		streamsToConvert: make(map[string]*bitmask.LongBitmask),
		jobs:             make(chan func()),
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create fsnotify watcher: %w", err)
	}
	mgr.startMonitoringConverters(watcher)

	// Lookup all available converter binaries
	entries, err := os.ReadDir(mgr.ConverterDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read converter directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := mgr.addConverter(filepath.Join(mgr.ConverterDir, entry.Name())); err != nil {
			return nil, fmt.Errorf("failed to add converter %q: %w", entry.Name(), err)
		}
	}

	tools.AssertFolderRWXPermissions("pcap_dir", pcapDir)
	tools.AssertFolderRWXPermissions("index_dir", indexDir)
	tools.AssertFolderRWXPermissions("snapshot_dir", snapshotDir)
	tools.AssertFolderRWXPermissions("state_dir", stateDir)

	// read all existing indexes and load them
	indexFileNames, err := tools.ListFiles(indexDir, "idx")
	if err != nil {
		return nil, err
	}
	for _, fn := range indexFileNames {
		idx, err := index.NewReader(fn)
		if err != nil {
			log.Printf("Unable to load index %q: %v", fn, err)
			continue
		}
		mgr.indexes = append(mgr.indexes, idx)
		mgr.nStreams += idx.StreamCount()
		mgr.nPackets += idx.PacketCount()
		if next := idx.MaxStreamID() + 1; mgr.nextStreamID < next {
			mgr.nextStreamID = next
		}
	}
	mgr.lock(mgr.indexes)

	stateFilenames, err := tools.ListFiles(stateDir, "state.json")
	if err != nil {
		return nil, err
	}
	stateTimestamp := time.Time{}
	cachedKnownPcapData := []*pcapmetadata.PcapInfo(nil)
	if mgr.nextStreamID != 0 {
		mgr.allStreams.Set(uint(mgr.nextStreamID - 1))
		for i := uint64(0); i != mgr.nextStreamID; i++ {
			mgr.allStreams.Set(uint(i))
		}
	}
nextStateFile:
	for _, fn := range stateFilenames {
		f, err := os.Open(fn)
		if err != nil {
			log.Printf("Unable to load state file %q: %v", fn, err)
			continue
		}
		s := stateFile{}
		if err := json.NewDecoder(f).Decode(&s); err != nil {
			log.Printf("Unable to parse state file %q: %v", fn, err)
			continue
		}
		if s.Saved.Before(stateTimestamp) {
			continue
		}
		newTags := make(map[string]*tag, len(s.Tags))
		for _, t := range s.Tags {
			q, err := query.Parse(t.Definition)
			if err != nil {
				log.Printf("Invalid tag %q in statefile %q: %v", t.Name, fn, err)
				continue nextStateFile
			}
			if _, ok := newTags[t.Name]; ok {
				log.Printf("Invalid tag %q in statefile %q: duplicate name", t.Name, fn)
				continue nextStateFile
			}
			nt := &tag{
				TagDetails: query.TagDetails{
					Uncertain:  mgr.allStreams,
					Conditions: q.Conditions,
				},
				definition: t.Definition,
				features:   q.Conditions.Features(),
				color:      t.Color,
			}
			if strings.HasPrefix(t.Name, "mark/") || strings.HasPrefix(t.Name, "generated/") {
				ids, ok := q.Conditions.StreamIDs(mgr.nextStreamID)
				if !ok {
					log.Printf("Invalid tag %q in statefile %q: 'mark' or 'generated' tag is malformed", t.Name, fn)
					continue nextStateFile
				}
				nt.Matches = ids
				nt.Uncertain = bitmask.LongBitmask{}
			}
			for _, converterName := range t.Converters {
				converter, ok := mgr.converters[converterName]
				if !ok {
					// TODO: just remove the cache file if any?
					log.Printf("Invalid tag %q in statefile %q: references non-existing converter %q", t.Name, fn, converterName)
					continue
				}
				if err := mgr.attachConverterToTag(nt, t.Name, converter); err != nil {
					log.Printf("Invalid tag %q in statefile %q: Failed to attach converter %q: %v", t.Name, fn, converterName, err)
				}
			}
			newTags[t.Name] = nt
		}
		cyclingTags := map[string]struct{}{}
		for n, t := range newTags {
			for _, tn := range t.referencedTags() {
				if n == tn {
					log.Printf("Invalid tag %q in statefile %q: references itself", n, fn)
					continue nextStateFile
				}
				if _, ok := newTags[tn]; !ok {
					log.Printf("Invalid tag %q in statefile %q: references non-existing tag %q", n, fn, tn)
					continue nextStateFile
				}
			}
			cyclingTags[n] = struct{}{}
		}
	checkCyclingTags:
		for {
		nextCyclingTag:
			for n := range cyclingTags {
				for _, rt := range newTags[n].referencedTags() {
					if _, ok := cyclingTags[rt]; ok {
						continue nextCyclingTag
					}
				}
				delete(cyclingTags, n)
				continue checkCyclingTags
			}
			for n := range cyclingTags {
				log.Printf("Invalid tag %q in statefile %q: contains cycle", n, fn)
				continue nextStateFile
			}
			break
		}
		mgr.tags = newTags
		mgr.stateFilename = fn
		stateTimestamp = s.Saved
		cachedKnownPcapData = s.Pcaps
	}

	mgr.builder, err = builder.New(pcapDir, indexDir, snapshotDir, cachedKnownPcapData)
	if err != nil {
		return nil, err
	}
	if len(mgr.builder.KnownPcaps()) != len(cachedKnownPcapData) {
		//nolint:errcheck
		mgr.saveState()
	}

	go func() {
		for f := range mgr.jobs {
			f()
		}
	}()
	mgr.jobs <- func() {
		mgr.startTaggingJobIfNeeded()
		mgr.startMergeJobIfNeeded()
	}
	return &mgr, nil
}

func (t tag) referencedTags() []string {
	return append(append([]string(nil), t.features.MainTags...), t.features.SubQueryTags...)
}

func (t tag) converterNames() []string {
	converterNames := make([]string, len(t.converters))
	for i, converter := range t.converters {
		converterNames[i] = converter.Name()
	}
	return converterNames
}

func (mgr *Manager) Close() {
	if mgr.watcher != nil {
		if err := mgr.watcher.Close(); err != nil {
			log.Printf("Failed to close watcher: %v", err)
		}
	}
	c := make(chan struct{})
	mgr.jobs <- func() {
		for _, converter := range mgr.converters {
			if err := converter.Close(); err != nil {
				log.Printf("Failed to close converter %q: %v", converter.Name(), err)
			}
		}
		close(c)
	}
	<-c
}

func (mgr *Manager) saveState() error {
	j := stateFile{
		Saved: time.Now(),
		Pcaps: mgr.builder.KnownPcaps(),
	}
	for n, t := range mgr.tags {
		j.Tags = append(j.Tags, struct {
			Name       string
			Definition string
			Color      string
			Converters []string
		}{
			Name:       n,
			Definition: t.definition,
			Color:      t.color,
			Converters: t.converterNames(),
		})
	}
	fn := tools.MakeFilename(mgr.StateDir, "state.json")
	f, err := os.Create(fn)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(&j); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if mgr.stateFilename != "" {
		if err := os.Remove(mgr.stateFilename); err != nil {
			log.Printf("Unable to delete old statefile %q: %v", mgr.stateFilename, err)
		}
	}
	mgr.stateFilename = fn
	return nil
}

func (mgr *Manager) inheritTagUncertainty() {
	resolvedTags := map[string]struct{}{}
	for len(resolvedTags) != len(mgr.tags) {
	outer:
		for tn, ti := range mgr.tags {
			if _, ok := resolvedTags[tn]; ok {
				continue
			}
			for _, rtn := range ti.referencedTags() {
				if _, ok := resolvedTags[rtn]; !ok {
					continue outer
				}
			}
			resolvedTags[tn] = struct{}{}
			if len(ti.features.MainTags) == 0 && len(ti.features.SubQueryTags) == 0 {
				continue
			}
			fullyInvalidated := false
			for _, rtn := range ti.features.SubQueryTags {
				if !mgr.tags[rtn].Uncertain.IsZero() {
					//TODO: is a matching stream really uncertain?
					ti.Uncertain = mgr.allStreams
					fullyInvalidated = true
					break
				}
			}
			if !fullyInvalidated {
				ti.Uncertain = ti.Uncertain.Copy()
				for _, rtn := range ti.features.MainTags {
					ti.Uncertain.Or(mgr.tags[rtn].Uncertain)
				}
			}
			mgr.tags[tn] = ti
		}
	}
}

func (mgr *Manager) invalidateTags(updatedStreams, addedStreams bitmask.LongBitmask) {
	for tn, ti := range mgr.tags {
		tin := *ti
		if ti.features.SubQueryFeatures != 0 {
			//TODO: is a matching stream really uncertain?
			tin.Uncertain = mgr.allStreams
		} else if ti.features.MainFeatures&^query.FeatureFilterID == 0 {
			continue
		} else {
			tin.Uncertain = ti.Uncertain.Copy()
			tin.Uncertain.Or(addedStreams)
			if ti.features.MainFeatures&(query.FeatureFilterData|query.FeatureFilterTimeAbsolute|query.FeatureFilterTimeRelative) != 0 {
				tin.Uncertain.Or(updatedStreams)
			}
		}
		mgr.tags[tn] = &tin
	}
	mgr.inheritTagUncertainty()
}

func (mgr *Manager) importPcapJob(filenames []string, nextStreamID uint64, existingIndexes []*index.Reader, existingIndexesReleaser indexReleaser) {
	processedFiles, createdIndexes, err := mgr.builder.FromPcap(mgr.PcapDir, filenames, existingIndexes)
	if err != nil {
		log.Printf("importPcapJob(%q) failed: %s", filenames, err)
	}
	updatedStreams := bitmask.LongBitmask{}
	addedStreams := bitmask.LongBitmask{}
	newStreamCount, newPacketCount := 0, 0
	newNextStreamID := nextStreamID
	for _, idx := range createdIndexes {
		newStreamCount += idx.StreamCount()
		newPacketCount += idx.PacketCount()
		if next := idx.MaxStreamID() + 1; newNextStreamID < next {
			newNextStreamID = next
		}
		for i := range idx.StreamIDs() {
			if i < nextStreamID {
				updatedStreams.Set(uint(i))
			} else {
				addedStreams.Set(uint(i))
			}
		}
	}
	allStreams := bitmask.LongBitmask{}
	if newNextStreamID != 0 {
		allStreams.Set(uint(newNextStreamID - 1))
		for i := uint64(0); i < newNextStreamID; i++ {
			allStreams.Set(uint(i))
		}
	}
	mgr.jobs <- func() {
		mgr.allStreams = allStreams
		existingIndexesReleaser.release(mgr)
		// add new indexes if some were created
		if len(createdIndexes) > 0 {
			mgr.indexes = append(mgr.indexes, createdIndexes...)
			mgr.nStreams += newStreamCount
			mgr.nPackets += newPacketCount
			mgr.nextStreamID = newNextStreamID
			mgr.lock(createdIndexes)
			mgr.addedStreamsDuringTaggingJob.Or(addedStreams)
			mgr.updatedStreamsDuringTaggingJob.Or(updatedStreams)
			mgr.invalidateTags(updatedStreams, addedStreams)
		}
		// remove finished job from queue
		mgr.importJobs = mgr.importJobs[processedFiles:]
		// start new import job if there are more queued
		if len(mgr.importJobs) >= 1 {
			idxs, rel := mgr.getIndexesCopy(0)
			go mgr.importPcapJob(mgr.importJobs[:], mgr.nextStreamID, idxs, rel)
		}
		mgr.startTaggingJobIfNeeded()
		mgr.startMergeJobIfNeeded()
		//nolint:errcheck
		mgr.saveState()
	}
}

func (mgr *Manager) startMergeJobIfNeeded() {
	if mgr.mergeJobRunning || mgr.taggingJobRunning {
		return
	}
	// only merge if all tags are on the newest version, prioritize updating tags
	for _, t := range mgr.tags {
		if !t.Uncertain.IsZero() {
			return
		}
	}
	nStreams := mgr.nStreams
	for i, idx := range mgr.indexes {
		c := idx.StreamCount()
		nStreams -= c
		if i >= mgr.nUnmergeableIndexes && c < nStreams {
			mgr.mergeJobRunning = true
			indexes, indexesReleaser := mgr.getIndexesCopy(i)
			go mgr.mergeIndexesJob(i, indexes, indexesReleaser)
			return
		}
	}
}

func (mgr *Manager) startTaggingJobIfNeeded() {
	if mgr.taggingJobRunning {
		return
	}
outer:
	for n, t := range mgr.tags {
		if t.Uncertain.IsZero() {
			continue
		}
		for _, tn := range t.referencedTags() {
			if !mgr.tags[tn].Uncertain.IsZero() {
				continue outer
			}
		}
		tagDetails := make(map[string]query.TagDetails)
		for _, tn := range t.referencedTags() {
			tagDetails[tn] = mgr.tags[tn].TagDetails
		}
		mgr.updatedStreamsDuringTaggingJob = bitmask.LongBitmask{}
		mgr.addedStreamsDuringTaggingJob = bitmask.LongBitmask{}
		mgr.taggingJobRunning = true
		indexes, releaser := mgr.getIndexesCopy(0)
		converters := make(map[string]index.ConverterAccess)
		for converterName, converter := range mgr.converters {
			converters[converterName] = converter
		}
		go mgr.updateTagJob(n, *t, tagDetails, converters, indexes, releaser)
		return
	}
}

func (mgr *Manager) mergeIndexesJob(offset int, indexes []*index.Reader, releaser indexReleaser) {
	mergedIndexes, err := index.Merge(mgr.IndexDir, indexes)
	if err != nil {
		indexFilenames := []string{}
		for _, i := range indexes {
			indexFilenames = append(indexFilenames, i.Filename())
		}
		log.Printf("mergeIndexesJob(%d, [%q]) failed: %s", offset, indexFilenames, err)
	}
	streamsDiff, packetsDiff := 0, 0
	for _, idx := range mergedIndexes {
		streamsDiff += idx.StreamCount()
		packetsDiff += idx.PacketCount()
	}
	for _, idx := range indexes {
		streamsDiff -= idx.StreamCount()
		packetsDiff -= idx.PacketCount()
	}
	mgr.jobs <- func() {
		// replace old indexes if successfully created
		if len(mergedIndexes) == 0 || err != nil {
			mgr.nUnmergeableIndexes++
		} else {
			rel := indexReleaser(mgr.indexes[offset : offset+len(indexes)])
			rel.release(mgr)
			mgr.lock(mergedIndexes)
			mgr.indexes = append(mgr.indexes[:offset], append(mergedIndexes, mgr.indexes[offset+len(indexes):]...)...)
			mgr.nUnmergeableIndexes += len(mergedIndexes) - 1
			mgr.nStreams += streamsDiff
			mgr.nPackets += packetsDiff
		}
		mgr.mergeJobRunning = false
		mgr.startMergeJobIfNeeded()
		releaser.release(mgr)
	}
}

func (mgr *Manager) updateTagJob(name string, t tag, tagDetails map[string]query.TagDetails, converters map[string]index.ConverterAccess, indexes []*index.Reader, releaser indexReleaser) {
	err := func() error {
		q, err := query.Parse(t.definition)
		if err != nil {
			return err
		}
		streams, _, err := index.SearchStreams(indexes, &t.Uncertain, q.ReferenceTime, q.Conditions, nil, []query.Sorting{{Key: query.SortingKeyID, Dir: query.SortingDirAscending}}, 0, 0, tagDetails, converters)
		if err != nil {
			return err
		}
		t.Matches = t.Matches.Copy()
		t.Matches.Sub(t.Uncertain)
		for _, s := range streams {
			t.Matches.Set(uint(s.ID()))
		}
		return nil
	}()
	if err != nil {
		log.Printf("updateTagJob failed: %q", err)
		t.Matches = bitmask.LongBitmask{}
	}
	t.Uncertain = bitmask.LongBitmask{}
	mgr.jobs <- func() {
		// don't touch the tag if it was modified
		if ot, ok := mgr.tags[name]; ok && ot.definition == t.definition {
			for _, converter := range t.converters {
				mgr.streamsToConvert[converter.Name()].Or(t.Matches)
			}
			mgr.tags[name] = &t
			if !(mgr.updatedStreamsDuringTaggingJob.IsZero() && mgr.addedStreamsDuringTaggingJob.IsZero()) {
				mgr.invalidateTags(mgr.updatedStreamsDuringTaggingJob, mgr.addedStreamsDuringTaggingJob)
			}
		}
		mgr.taggingJobRunning = false
		mgr.startTaggingJobIfNeeded()
		mgr.startMergeJobIfNeeded()
		mgr.startConverterJobIfNeeded()
		releaser.release(mgr)
	}
}

func (mgr *Manager) ImportPcap(filename string) {
	mgr.jobs <- func() {
		//add job to be processed by importer goroutine
		mgr.importJobs = append(mgr.importJobs, filename)
		//start import job when none running
		if len(mgr.importJobs) == 1 {
			indexes, releaser := mgr.getIndexesCopy(0)
			go mgr.importPcapJob(mgr.importJobs[:1], mgr.nextStreamID, indexes, releaser)
		}
	}
}

func (mgr *Manager) getIndexesCopy(start int) ([]*index.Reader, indexReleaser) {
	indexes := append([]*index.Reader(nil), mgr.indexes[start:]...)
	return indexes, mgr.lock(indexes)
}

func (mgr *Manager) Status() Statistics {
	c := make(chan Statistics)
	mgr.jobs <- func() {
		locks := uint(0)
		for _, n := range mgr.usedIndexes {
			locks += n
		}
		c <- Statistics{
			IndexCount:          len(mgr.indexes),
			IndexLockCount:      locks,
			PcapCount:           len(mgr.builder.KnownPcaps()),
			ImportJobCount:      len(mgr.importJobs),
			StreamCount:         mgr.nStreams,
			PacketCount:         mgr.nPackets,
			MergeJobRunning:     mgr.mergeJobRunning,
			TaggingJobRunning:   mgr.taggingJobRunning,
			ConverterJobRunning: mgr.converterJobRunning,
		}
		close(c)
	}
	res := <-c
	return res
}

func (mgr *Manager) KnownPcaps() []pcapmetadata.PcapInfo {
	c := make(chan []pcapmetadata.PcapInfo)
	mgr.jobs <- func() {
		r := []pcapmetadata.PcapInfo{}
		for _, p := range mgr.builder.KnownPcaps() {
			r = append(r, *p)
		}
		c <- r
		close(c)
	}
	res := <-c
	return res
}

func (mgr *Manager) ListTags() []TagInfo {
	c := make(chan []TagInfo)
	mgr.jobs <- func() {
		res := []TagInfo{}
		referencedTag := map[string]struct{}{}
		for _, t := range mgr.tags {
			for _, tn := range t.referencedTags() {
				referencedTag[tn] = struct{}{}
			}
		}
		for name, t := range mgr.tags {
			m := t.Matches.Copy()
			m.Sub(t.Uncertain)
			_, referenced := referencedTag[name]
			res = append(res, TagInfo{
				Name:           name,
				Definition:     t.definition,
				Color:          t.color,
				MatchingCount:  uint(m.OnesCount()),
				UncertainCount: uint(t.Uncertain.OnesCount()),
				Referenced:     referenced,
				Converters:     t.converterNames(),
			})
		}
		sort.Slice(res, func(i, j int) bool {
			return res[i].Name < res[j].Name
		})
		c <- res
		close(c)
	}
	return <-c
}

func (mgr *Manager) AddTag(name, color, queryString string) error {
	isMark := strings.HasPrefix(name, "mark/") || strings.HasPrefix(name, "generated/")
	if !(strings.HasPrefix(name, "tag/") || strings.HasPrefix(name, "service/") || isMark) {
		return errors.New("invalid tag name (need a 'tag/', 'service/', 'mark/' or 'generated/' prefix)")
	}
	if sub := strings.SplitN(name, "/", 2)[1]; sub == "" {
		return errors.New("invalid tag name (prefix only not allowed)")
	}
	q, err := query.Parse(queryString)
	if err != nil {
		return err
	}
	features := q.Conditions.Features()
	if (features.MainFeatures|features.SubQueryFeatures)&query.FeatureFilterTimeRelative != 0 {
		return errors.New("relative times not yet supported in tags")
	}
	if q.Grouping != nil {
		return errors.New("grouping not allowed in tags")
	}
	nt := &tag{
		TagDetails: query.TagDetails{
			Conditions: q.Conditions,
		},
		definition: queryString,
		features:   features,
		color:      color,
	}
	for _, tn := range nt.referencedTags() {
		if tn == name {
			return errors.New("self reference not allowed in tags")
		}
	}
	if isMark {
		if _, ok := q.Conditions.StreamIDs(0); !ok {
			return errors.New("tags of type `mark` have to only contain an `id` filter")
		}
	}
	c := make(chan error)
	mgr.jobs <- func() {
		err := func() error {
			if _, ok := mgr.tags[name]; ok {
				return errors.New("tag already exists")
			}
			// check if all referenced tags exist
			for _, t := range nt.referencedTags() {
				if _, ok := mgr.tags[t]; !ok {
					return fmt.Errorf("unknown referenced tag %q", t)
				}
			}
			if isMark {
				nt.Matches, _ = q.Conditions.StreamIDs(mgr.nextStreamID)
			} else {
				nt.Uncertain = mgr.allStreams
			}
			mgr.tags[name] = nt
			if !isMark {
				mgr.startTaggingJobIfNeeded()
			}
			return nil
		}()
		c <- err
		close(c)
		//nolint:errcheck
		mgr.saveState()
	}
	return <-c
}

func (mgr *Manager) DelTag(name string) error {
	c := make(chan error)
	mgr.jobs <- func() {
		err := func() error {
			tag, ok := mgr.tags[name]
			if !ok {
				return fmt.Errorf("unknown tag %q", name)
			}
			for t2name, t2 := range mgr.tags {
				for _, tn := range t2.referencedTags() {
					if tn == name {
						return fmt.Errorf("tag %q still references the tag to be deleted", t2name)
					}
				}
			}
			// remove converter results of attached converters from cache
			if len(tag.converters) > 0 {
				for _, converter := range tag.converters {
					if err := mgr.detachConverterFromTag(tag, name, converter); err != nil {
						return err
					}
				}
			}
			delete(mgr.tags, name)
			return nil
		}()
		c <- err
		close(c)
		//nolint:errcheck
		mgr.saveState()
	}
	return <-c
}

func UpdateTagOperationMarkAddStream(streams []uint64) UpdateTagOperation {
	s := make([]uint64, 0, len(streams))
	s = append(s, streams...)
	return func(i *updateTagOperationInfo) {
		i.markTagAddStreams = s
	}
}

func UpdateTagOperationMarkDelStream(streams []uint64) UpdateTagOperation {
	s := make([]uint64, 0, len(streams))
	s = append(s, streams...)
	return func(i *updateTagOperationInfo) {
		i.markTagDelStreams = s
	}
}

func UpdateTagOperationUpdateColor(color string) UpdateTagOperation {
	return func(i *updateTagOperationInfo) {
		i.color = color
	}
}

func UpdateTagOperationSetConverter(converterNames []string) UpdateTagOperation {
	return func(i *updateTagOperationInfo) {
		i.setConverterNames = converterNames
		i.convertersUpdated = true
	}
}

func (mgr *Manager) UpdateTag(name string, operation UpdateTagOperation) error {
	info := updateTagOperationInfo{convertersUpdated: false}
	operation(&info)
	maxUsedStreamID := uint64(0)
	if len(info.markTagAddStreams) != 0 || len(info.markTagDelStreams) != 0 {
		if !(strings.HasPrefix(name, "mark/") || strings.HasPrefix(name, "generated/")) {
			return fmt.Errorf("tag %q is not of type 'mark' or 'enerated'", name)
		}
		for _, s := range info.markTagAddStreams {
			if maxUsedStreamID <= s {
				maxUsedStreamID = s + 1
			}
		}
		for _, s := range info.markTagDelStreams {
			if maxUsedStreamID <= s {
				maxUsedStreamID = s + 1
			}
		}
		if maxUsedStreamID == 0 {
			// no operation
			return nil
		}
		maxUsedStreamID--
	}
	c := make(chan error)
	mgr.jobs <- func() {
		err := func() error {
			tag, ok := mgr.tags[name]
			if !ok {
				return fmt.Errorf("unknown tag %q", name)
			}
			if info.color != "" {
				tag.color = info.color
			}
			if info.convertersUpdated {
				// detach deselected converters from tag
				for _, converter := range tag.converters {
					if slices.Contains(info.setConverterNames, converter.Name()) {
						continue
					}
					if err := mgr.detachConverterFromTag(tag, name, converter); err != nil {
						return fmt.Errorf("failed to detach converter %q from tag %q: %w", converter.Name(), name, err)
					}
				}
				// attach new converters to tag
				converterNames := tag.converterNames()
				for _, converterName := range info.setConverterNames {
					if slices.Contains(converterNames, converterName) {
						continue
					}
					if converter, ok := mgr.converters[converterName]; !ok {
						return fmt.Errorf("unknown converter %q", converterName)
					} else {
						if err := mgr.attachConverterToTag(tag, name, converter); err != nil {
							return fmt.Errorf("failed to attach converter %q to tag %q: %w", converterName, name, err)
						}
					}
				}
				mgr.saveState()
			}
			if maxUsedStreamID != 0 {
				if maxUsedStreamID >= mgr.nextStreamID {
					return fmt.Errorf("unknown stream id %d", maxUsedStreamID)
				}
				newTag := *tag
				newTag.Matches = tag.Matches.Copy()
				for _, s := range info.markTagAddStreams {
					newTag.Matches.Set(uint(s))
					newTag.Uncertain.Set(uint(s))
				}
				for _, s := range info.markTagDelStreams {
					newTag.Matches.Unset(uint(s))
					newTag.Uncertain.Set(uint(s))
				}

				b := strings.Builder{}
				if newTag.Matches.IsZero() {
					b.WriteString("id:-1")
				} else {
					b.WriteString("id:")
					last := uint(0)
					for {
						zeros := newTag.Matches.TrailingZerosFrom(last)
						if zeros < 0 {
							break
						}
						if last != 0 {
							b.WriteByte(',')
						}
						last += uint(zeros)
						b.WriteString(fmt.Sprintf("%d", last))
						last++
					}
				}
				newTag.definition = b.String()
				if q, err := query.Parse(newTag.definition); err == nil {
					newTag.Conditions = q.Conditions
				}
				mgr.tags[name] = &newTag
				mgr.inheritTagUncertainty()
				mgr.tags[name].Uncertain = bitmask.LongBitmask{}
				mgr.startTaggingJobIfNeeded()
			}
			return nil
		}()
		c <- err
		close(c)
		//nolint:errcheck
		mgr.saveState()
	}
	return <-c
}

func (mgr *Manager) lock(indexes []*index.Reader) indexReleaser {
	for _, i := range indexes {
		mgr.usedIndexes[i]++
	}
	return indexReleaser(append([]*index.Reader(nil), indexes...))
}

// release all contained indexes from within the mgr goroutine
func (r *indexReleaser) release(mgr *Manager) {
	for _, i := range *r {
		mgr.usedIndexes[i]--
		if mgr.usedIndexes[i] == 0 {
			delete(mgr.usedIndexes, i)
			i.Close()
			os.Remove(i.Filename())
		}
	}
}

func (mgr *Manager) startConverterJobIfNeeded() {
	if mgr.converterJobRunning {
		return
	}
	// TODO: limit number of concurrent jobs, spread across all converters
	for converterName, converter := range mgr.converters {
		if mgr.streamsToConvert[converterName].IsZero() {
			continue
		}

		mgr.converterJobRunning = true
		indexes, releaser := mgr.getIndexesCopy(0)
		// process whole work queue in one job and reset it here now.
		// otherwise streams could get updated while the job is running
		// but we wouldn't run the converter again.
		workQueue := mgr.streamsToConvert[converterName].Copy()
		mgr.streamsToConvert[converterName] = &bitmask.LongBitmask{}
		go mgr.convertStreamJob(converter, workQueue, indexes, releaser)
		break
	}
}

func (mgr *Manager) convertStreamJob(converter *converters.CachedConverter, streamIDs bitmask.LongBitmask, indexes []*index.Reader, releaser indexReleaser) {
	convertedStreamIDs, err := func() (bitmask.LongBitmask, error) {
		type result struct {
			streamID uint64
			err      error
		}
		numCPU := uint64(runtime.NumCPU())
		resultChannel := make(chan result, numCPU)
		convertedCount := uint64(0)
		convertedStreamIDs := bitmask.LongBitmask{}

		waitForResults := func(limit uint64) error {
			lastError := error(nil)
			for i := uint64(0); i < limit; i++ {
				res, ok := <-resultChannel
				if !ok {
					lastError = fmt.Errorf("unexpected end of result channel")
				}
				if res.err != nil {
					lastError = res.err
				} else {
					convertedStreamIDs.Set(uint(res.streamID))
				}
			}
			return lastError
		}

		for idxIdx := len(indexes) - 1; idxIdx >= 0; idxIdx-- {
			index := indexes[idxIdx]
			streamID := index.MinStreamID()
			for {
				zeros := streamIDs.TrailingZerosFrom(uint(streamID))
				if zeros < 0 {
					break
				}
				streamID += uint64(zeros)
				if streamID > index.MaxStreamID() {
					break
				}

				// Already have that result
				if converter.Contains(streamID) {
					streamIDs.Unset(uint(streamID))
					continue
				}

				// Load the stream from the index
				stream, err := index.StreamByID(streamID)
				streamID++
				if err != nil {
					waitForResults(convertedCount)
					close(resultChannel)
					return convertedStreamIDs, err
				}
				// The stream isn't in this index file
				if stream == nil {
					continue
				}

				// Convert the stream
				go func() {
					_, _, _, err := converter.Data(stream)
					resultChannel <- result{streamID: stream.ID(), err: err}
				}()
				streamIDs.Unset(uint(streamID))
				convertedCount++

				if convertedCount%uint64(numCPU) == 0 {
					lastError := waitForResults(uint64(numCPU))
					convertedCount -= uint64(numCPU)
					if lastError != nil {
						close(resultChannel)
						return convertedStreamIDs, lastError
					}
				}
			}

			lastError := waitForResults(convertedCount)
			if lastError != nil {
				close(resultChannel)
				return convertedStreamIDs, lastError
			}
		}
		close(resultChannel)
		return convertedStreamIDs, nil
	}()

	if err != nil {
		log.Printf("error while converting stream: %v", err)
	}

	mgr.jobs <- func() {
		mgr.converterJobRunning = false

		// The converter was removed while we were running.
		// Discard the result.
		if _, ok := mgr.converters[converter.Name()]; !ok {
			if err := converter.Reset(); err != nil {
				log.Printf("error while resetting converter after discarding results: %v", err)
			}

			mgr.startConverterJobIfNeeded()
			releaser.release(mgr)
			return
		}

		if !streamIDs.IsZero() {
			// Some streams were not converted.
			// Add them to the work queue again.
			mgr.streamsToConvert[converter.Name()].Or(streamIDs)
		}

		// Mark the converted streams as uncertain on all tags using a data: filter
		// The tag could match on the converted data now.
		for _, tag := range mgr.tags {
			// TODO: Only tag again if the tag matches converted data
			if tag.features.MainFeatures&query.FeatureFilterData == 0 && tag.features.SubQueryFeatures&query.FeatureFilterData == 0 {
				continue
			}
			tag.Uncertain.Or(convertedStreamIDs)
		}
		mgr.startTaggingJobIfNeeded()
		mgr.startConverterJobIfNeeded()
		releaser.release(mgr)
	}
}

func (mgr *Manager) startMonitoringConverters(watcher *fsnotify.Watcher) {
	go func() {
		var (
			// Wait 500ms for new events; each new event resets the timer.
			waitFor = 500 * time.Millisecond

			// Keep track of the timers, as path → timer.
			mu     sync.Mutex
			timers = make(map[string]*time.Timer)
		)
		for {
			select {
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)

			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				log.Println("event:", event)

				if event.Has(fsnotify.Remove) {
					mgr.jobs <- func() {
						if err := mgr.removeConverter(event.Name); err != nil {
							log.Printf("error while removing converter: %v", err)
						}
					}
				}

				if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
					continue
				}

				mu.Lock()
				timer, ok := timers[event.Name]
				mu.Unlock()

				// No timer yet, so create one.
				if !ok {
					timer = time.AfterFunc(math.MaxInt64, func() {
						mu.Lock()
						delete(timers, event.Name)
						mu.Unlock()

						mgr.jobs <- func() {
							if event.Has(fsnotify.Create) {
								if err := mgr.addConverter(event.Name); err != nil {
									log.Printf("error while adding converter: %v", err)
								}
							}
							if event.Has(fsnotify.Write) {
								if err := mgr.restartConverterProcess(event.Name); err != nil {
									log.Printf("error while restarting converter: %v", err)
								}
							}
						}
					})
					timer.Stop()

					mu.Lock()
					timers[event.Name] = timer
					mu.Unlock()
				}

				// Reset the timer for this path, so it will start again.
				timer.Reset(waitFor)
			}
		}
	}()

	err := watcher.Add(mgr.ConverterDir)
	if err != nil {
		log.Fatal(err)
	}
}

func (mgr *Manager) addConverter(path string) error {
	// TODO: Do we want to check this now or when we start the converter?
	err := unix.Access(path, unix.X_OK)
	if err != nil {
		return fmt.Errorf("error: converter %s is not executable", path)
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if _, ok := mgr.converters[name]; ok {
		return fmt.Errorf("error: converter %s already exists", name)
	}
	if name == "none" {
		return fmt.Errorf("error: converter %s is reserved", name)
	}
	// Converter names have to be plain ascii so we can use them in the query language easily.
	if !regexp.MustCompile(`^[a-zA-Z0-9_]+$`).MatchString(name) {
		return fmt.Errorf("error: converter %s has to be alphanumeric", name)
	}

	converter, err := converters.NewCache(name, path, mgr.IndexDir)
	if err != nil {
		return fmt.Errorf("error: failed to create converter %s: %w", name, err)
	}
	mgr.converters[name] = converter
	mgr.streamsToConvert[name] = &bitmask.LongBitmask{}
	return nil
}

func (mgr *Manager) removeConverter(path string) error {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	converter, ok := mgr.converters[name]
	if !ok {
		return fmt.Errorf("error: converter %s does not exist", name)
	}

	// remove converter from all tags
	for _, t := range mgr.tags {
		if err := mgr.detachConverterFromTag(t, name, converter); err != nil {
			return err
		}
	}

	// Stop the process if it is running and delete the cache file.
	if err := converter.Reset(); err != nil {
		return err
	}

	delete(mgr.converters, name)
	delete(mgr.streamsToConvert, name)
	mgr.saveState()
	return nil
}

func (mgr *Manager) restartConverterProcess(path string) error {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	converter, ok := mgr.converters[name]
	if !ok {
		if err := mgr.addConverter(path); err != nil {
			return err
		}
	}
	// Stop the process if it is running and restart it
	if err := converter.Reset(); err != nil {
		return err
	}

	// run the converter on all streams that match the tags it is attached to again
	for _, tag := range mgr.tags {
		if slices.Contains(tag.converters, converter) {
			mgr.streamsToConvert[name].Or(tag.Matches)
		}
	}
	mgr.startConverterJobIfNeeded()

	return nil
}

func (mgr *Manager) attachConverterToTag(tag *tag, tagName string, converter *converters.CachedConverter) error {
	// check if converter already exists
	if slices.Contains(tag.converters, converter) {
		return nil
	}
	// assert low complexity of this tag's query
	// cannot attach converter to tag which references other tags or matches on stream data
	// because we don't want to recursively trigger converters
	// TODO: we could allow data queries if they only reference the stream's own plain data
	if tag.features.MainFeatures&query.FeatureFilterData != 0 || tag.features.SubQueryFeatures&query.FeatureFilterData != 0 || len(tag.features.MainTags) > 0 || len(tag.features.SubQueryTags) > 0 {
		return fmt.Errorf("error: cannot attach converter to tag %s because it's query is too complex", tagName)
	}

	tag.converters = append(tag.converters, converter)
	mgr.streamsToConvert[converter.Name()].Or(tag.Matches)
	mgr.startConverterJobIfNeeded()
	return nil
}

func (mgr *Manager) detachConverterFromTag(tag *tag, tagName string, converter *converters.CachedConverter) error {
	for i, c := range tag.converters {
		if c == converter {
			tag.converters = append(tag.converters[:i], tag.converters[i+1:]...)
			break
		}
	}
	// delete/invalidate converter results for all matching streams now
	// but only if they aren't matches of other tags the converter is attached to.
	matchingStreams := bitmask.LongBitmask{}
	for _, t := range mgr.tags {
		if t == tag {
			continue
		}
		if slices.Contains(t.converters, converter) {
			matchingStreams.Or(t.Matches)
		}
	}

	// only delete results for streams that are not matched by other tags
	onlyThisTag := tag.Matches.Copy()
	onlyThisTag.Sub(matchingStreams)
	mgr.streamsToConvert[converter.Name()].Sub(onlyThisTag)
	// TODO: invalidate all streams in the cache that are only matched by this tag.

	if matchingStreams.IsZero() {
		// no other tags use this converter, delete all results
		if err := converter.Reset(); err != nil {
			return err
		}
	}
	return nil
}

func (mgr *Manager) ResetConverter(converterName string) error {
	c := make(chan error)
	mgr.jobs <- func() {
		c <- mgr.restartConverterProcess(converterName)
		close(c)
	}
	return <-c
}

func (mgr *Manager) ListConverters() []*converters.Statistics {
	c := make(chan []*converters.Statistics)
	mgr.jobs <- func() {
		stats := make([]*converters.Statistics, 0, len(mgr.converters))
		for _, converter := range mgr.converters {
			stats = append(stats, converter.Statistics())
		}
		c <- stats
		close(c)
	}
	return <-c
}

func (mgr *Manager) ConverterStderr(converterName string) ([][]string, error) {
	c := make(chan [][]string)
	mgr.jobs <- func() {
		converter, ok := mgr.converters[converterName]
		if !ok {
			c <- nil
			close(c)
			return
		}
		c <- converter.Stderrs()
		close(c)
	}
	stderrs := <-c
	if stderrs == nil {
		return nil, fmt.Errorf("error: converter %s does not exist", converterName)
	}
	return stderrs, nil
}

func (mgr *Manager) GetView() View {
	return View{mgr: mgr}
}

func (v *View) fetch() error {
	if len(v.indexes) != 0 {
		return nil
	}
	v.tagDetails = make(map[string]query.TagDetails)
	v.converters = make(map[string]index.ConverterAccess)
	c := make(chan error)
	v.mgr.jobs <- func() {
		v.indexes, v.releaser = v.mgr.getIndexesCopy(0)
		for tn, ti := range v.mgr.tags {
			v.tagDetails[tn] = ti.TagDetails
		}
		for converterName, converter := range v.mgr.converters {
			v.converters[converterName] = converter
		}
		c <- nil
		close(c)
	}
	return <-c
}

func (v *View) Release() {
	if len(v.releaser) != 0 {
		v.mgr.jobs <- func() {
			v.releaser.release(v.mgr)
		}
	}
}

func PrefetchTags(tags []string) StreamsOption {
	return func(o *streamsOptions) {
		o.prefetchTags = append(o.prefetchTags, tags...)
	}
}

func PrefetchAllTags() StreamsOption {
	return func(o *streamsOptions) {
		o.prefetchAllTags = true
	}
}

func Limit(defaultLimit, page uint) StreamsOption {
	return func(o *streamsOptions) {
		o.defaultLimit = defaultLimit
		o.page = page
	}
}

func (v *View) prefetchTags(tags []string, bm bitmask.LongBitmask) error {
	if len(tags) == 0 {
		return nil
	}
	uncertainTags := map[string]bitmask.LongBitmask{}
	addTag := (func(string, bitmask.LongBitmask))(nil)
	addTag = func(tn string, streams bitmask.LongBitmask) {
		ti := v.tagDetails[tn]
		if ti.Uncertain.IsZero() {
			return
		}
		uncertain := ti.Uncertain
		if !streams.IsZero() {
			uncertain = uncertain.Copy()
			uncertain.And(streams)
			if uncertain.IsZero() {
				return
			}
		}
		if u, ok := uncertainTags[tn]; ok {
			tmp := uncertain.Copy()
			tmp.Sub(u)
			if tmp.IsZero() {
				return
			}
			tmp.Or(u)
			uncertain = tmp
		}
		uncertainTags[tn] = uncertain
		f := ti.Conditions.Features()
		for _, tn := range f.SubQueryTags {
			addTag(tn, bitmask.LongBitmask{})
		}
		for _, tn := range f.MainTags {
			addTag(tn, uncertain)
		}
	}
	for _, tn := range tags {
		if _, ok := v.tagDetails[tn]; !ok {
			return fmt.Errorf("tag %q not defined", tn)
		}
		addTag(tn, bm)
	}
	for len(uncertainTags) != 0 {
	outer:
		for tn, uncertain := range uncertainTags {
			ti := v.tagDetails[tn]
			f := ti.Conditions.Features()
			for _, rtn := range f.MainTags {
				if _, ok := uncertainTags[rtn]; ok {
					continue outer
				}
			}
			for _, rtn := range f.SubQueryTags {
				if _, ok := uncertainTags[rtn]; ok {
					continue outer
				}
			}
			matches, _, err := index.SearchStreams(v.indexes, &uncertain, time.Time{}, ti.Conditions, nil, []query.Sorting{{Key: query.SortingKeyID, Dir: query.SortingDirAscending}}, 0, 0, v.tagDetails, v.converters)
			if err != nil {
				return err
			}
			ti.Uncertain = ti.Uncertain.Copy()
			ti.Uncertain.Sub(uncertain)
			ti.Matches = ti.Matches.Copy()
			ti.Matches.Sub(uncertain)
			for _, s := range matches {
				ti.Matches.Set(uint(s.StreamID))
			}
			v.tagDetails[tn] = ti
			delete(uncertainTags, tn)
		}
	}
	return nil
}

func (v *View) AllStreams(f func(StreamContext) error, options ...StreamsOption) error {
	opts := streamsOptions{}
	for _, o := range options {
		o(&opts)
	}
	if opts.defaultLimit != 0 || opts.page != 0 {
		return errors.New("Limit not supported for AllStreams")
	}
	if err := v.fetch(); err != nil {
		return err
	}
	if opts.prefetchAllTags {
		for tn := range v.tagDetails {
			opts.prefetchTags = append(opts.prefetchTags, tn)
		}
	}
	v.prefetchTags(opts.prefetchTags, bitmask.LongBitmask{})
	for i := len(v.indexes); i > 0; i-- {
		idx := v.indexes[i-1]
		if err := idx.AllStreams(func(s *index.Stream) error {
			for _, idx2 := range v.indexes[i:] {
				if _, ok := idx2.StreamIDs()[s.ID()]; ok {
					return nil
				}
			}
			return f(StreamContext{
				s: s,
				v: v,
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func (v *View) SearchStreams(filter *query.Query, f func(StreamContext) error, options ...StreamsOption) (bool, uint, error) {
	opts := streamsOptions{}
	for _, o := range options {
		o(&opts)
	}
	if err := v.fetch(); err != nil {
		return false, 0, err
	}
	if opts.prefetchAllTags {
		for tn := range v.tagDetails {
			opts.prefetchTags = append(opts.prefetchTags, tn)
		}
	}
	limit := opts.defaultLimit
	if filter.Limit != nil {
		limit = *filter.Limit
	}
	offset := opts.page * limit
	res, hasMore, err := index.SearchStreams(v.indexes, nil, filter.ReferenceTime, filter.Conditions, filter.Grouping, filter.Sorting, limit, offset, v.tagDetails, v.converters)
	if err != nil {
		return false, 0, err
	}
	if len(res) == 0 {
		return hasMore, offset, nil
	}
	if len(opts.prefetchTags) != 0 {
		searchedStreams := bitmask.LongBitmask{}
		for _, s := range res {
			searchedStreams.Set(uint(s.StreamID))
		}
		if err := v.prefetchTags(opts.prefetchTags, searchedStreams); err != nil {
			return false, 0, err
		}
	}
	for _, s := range res {
		if err := f(StreamContext{
			s: s,
			v: v,
		}); err != nil {
			return false, 0, err
		}
	}
	return hasMore, offset, nil
}

func (v *View) ReferenceTime() (time.Time, error) {
	if err := v.fetch(); err != nil {
		return time.Time{}, err
	}
	referenceTime := time.Time{}
	for _, idx := range v.indexes {
		if referenceTime.IsZero() || referenceTime.After(idx.ReferenceTime) {
			referenceTime = idx.ReferenceTime
		}
	}
	return referenceTime, nil
}

func (v *View) Stream(streamID uint64) (StreamContext, error) {
	if err := v.fetch(); err != nil {
		return StreamContext{}, err
	}
	for i := len(v.indexes) - 1; i >= 0; i-- {
		idx := v.indexes[i]
		stream, err := idx.StreamByID(streamID)
		if err != nil {
			return StreamContext{}, err
		}
		if stream == nil {
			continue
		}
		return StreamContext{
			s: stream,
			v: v,
		}, nil
	}
	return StreamContext{}, nil
}

func (c StreamContext) Stream() *index.Stream {
	return c.s
}

func (c StreamContext) Data(converterName string) ([]index.Data, error) {
	if c.Stream() == nil {
		return nil, fmt.Errorf("stream not found")
	}
	if converterName == "" {
		return c.Stream().Data()
	}
	converter, ok := c.v.converters[converterName]
	if !ok {
		return nil, fmt.Errorf("invalid converter %q", converterName)
	}
	data, _, _, err := converter.Data(c.Stream())
	return data, err
}

func (c StreamContext) HasTag(name string) (bool, error) {
	td := c.v.tagDetails[name]
	if !td.Uncertain.IsSet(uint(c.s.ID())) {
		return td.Matches.IsSet(uint(c.s.ID())), nil
	}
	//TODO: figure out if the uncertain tag matches
	return false, nil
}

func (c StreamContext) AllTags() ([]string, error) {
	tags := []string{}
	for tn, td := range c.v.tagDetails {
		if !td.Uncertain.IsSet(uint(c.s.ID())) {
			if td.Matches.IsSet(uint(c.s.ID())) {
				tags = append(tags, tn)
			}
			continue
		}
		//TODO: figure out if the uncertain tag matches
	}
	sort.Strings(tags)
	return tags, nil
}

func (c StreamContext) AllConverters() ([]string, error) {
	converters := []string{}
	for tn := range c.v.tagDetails {
		ok, err := c.HasTag(tn)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// FIXME: don't reference mgr here, but cache this info in the view
		tag := c.v.mgr.tags[tn]
		for _, converter := range tag.converters {
			if !slices.Contains(converters, converter.Name()) {
				converters = append(converters, converter.Name())
			}
		}
	}
	sort.Strings(converters)
	return converters, nil
}
