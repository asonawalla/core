// gazconsumer lol
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	etcd "github.com/coreos/etcd/client"
	"github.com/dustin/go-humanize"
	"github.com/olekukonko/tablewriter"

	"io"

	"github.com/pippio/consensus"
	"github.com/pippio/endpoints"
	"github.com/pippio/gazette/gazette"
	"github.com/pippio/gazette/journal"
	"github.com/pippio/varz"
)

const (
	bytesPerGigabyte    = 1024 * 1024 * 1024
	journalNotBeingRead = -1
)

var (
	monitor         = flag.Bool("monitor", false, "Use arguments to provide metrics to Prometheus.")
	monitorInterval = flag.Duration("monitorInterval", time.Minute, "How often to update metrics.")
	prefixList      prefixFlagSet

	// Global service objects.
	keysAPI       etcd.KeysAPI
	gazetteClient *gazette.Client
	httpClient    *http.Client
)

type varsData struct {
	Gazette *gazetteData `json:"gazette"`
}

type gazetteData struct {
	Readers map[string]*readerData `json:"readers"`
}

type readerData struct {
	Bytes int64 `json:"bytes"`
	Head  int64 `json:"head"`
}

type memberData struct {
	masters, replicas int
	states            map[string]string
	hasUnreadJournals bool
	totalLag          int64
}

type consumerData struct {
	memberNames  []string
	journalNames []string
	members      map[string]memberData
	owners       map[string]string

	journalLag map[string]journalData
}

type journalData struct {
	lag   int64
	state string
}

func (cd consumerData) TotalLag() int64 {
	var res int64 = 0
	for _, data := range cd.journalLag {
		res += data.lag
	}
	return res
}

func main() {
	flag.Var(&prefixList, "prefix", "Specify an Etcd prefix to check for consumers.")
	defer varz.Initialize("gazconsumer").Cleanup()

	httpClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: gazette.MakeHttpTransport(),
	}

	var err error
	gazetteClient, err = gazette.NewClient(*endpoints.GazetteEndpoint)
	if err != nil {
		log.WithField("err", err).Fatal("failed to init gazette client")
	}
	etcdClient, err := etcd.New(etcd.Config{
		Endpoints: []string{"http://" + *endpoints.EtcdEndpoint}})
	if err != nil {
		log.WithField("err", err).Fatal("failed to init etcd client")
	}
	keysAPI = etcd.NewKeysAPI(etcdClient)

	// Expand |prefixList| into |consumerList|.
	var consumerList = expandPrefixList(prefixList)

	checkConsumers(consumerList)
	if *monitor {
		for _ = range time.Tick(*monitorInterval) {
			consumerList = expandPrefixList(prefixList)
			checkConsumers(consumerList)
		}
	}
}

func checkConsumers(consumerList []string) {
	for _, consumer := range consumerList {
		var cdata = makeConsumerData(consumer)
		// Do not log statistics for consumers with 0 members.
		if *monitor {
			if len(cdata.memberNames) > 0 {
				varz.ObtainCount("gazconsumer", "lag", "#consumer", consumer).Set(cdata.TotalLag())
			}
		} else {
			fmt.Printf("Consumer: %s\n\n", consumer)
			printLong(cdata, os.Stdout)
		}
	}
}

func printLong(cdata consumerData, out io.Writer) {
	var journalsTable = tablewriter.NewWriter(out)
	journalsTable.SetHeader([]string{"Journal", "Lag", "State", "Owner"})
	sort.Strings(cdata.journalNames)
	for _, journal := range cdata.journalNames {
		var owner = cdata.owners[journal]
		if data, ok := cdata.journalLag[journal]; !ok {
			journalsTable.Append([]string{journal, "unknown", data.state, owner})
		} else {
			journalsTable.Append([]string{journal, humanize.Bytes(uint64(data.lag)), data.state, owner})
		}
	}
	journalsTable.Render()
	var membersTable = tablewriter.NewWriter(out)
	membersTable.SetHeader([]string{"Member", "Masters", "Replicas", "Total Lag"})
	sort.Strings(cdata.memberNames)
	for _, member := range cdata.memberNames {
		var info = cdata.members[member]
		var lagFmt = "%s"
		if info.hasUnreadJournals {
			lagFmt = ">= %s (incomplete data)"
		}

		membersTable.Append([]string{
			member, strconv.Itoa(info.masters), strconv.Itoa(info.replicas),
			fmt.Sprintf(lagFmt, humanize.Bytes(uint64(info.totalLag)))})
	}
	membersTable.Render()
}

func makeConsumerData(consumerPath string) consumerData {
	var key = path.Join(consumerPath, "items")
	shardsRoot, err := keysAPI.Get(context.Background(), key,
		&etcd.GetOptions{Recursive: true, Sort: true})
	if err != nil {
		log.WithFields(log.Fields{"err": err, "key": key}).Fatal("can't load etcd key")
	}
	key = path.Join(consumerPath, "offsets")

	// Optional: Use Etcd offsets of the consumer to produce a maximum lag that
	// can be relied upon even for shards that are in-transition. If the offsets
	// don't exist, carry on.
	offsetsRoot, _ := keysAPI.Get(context.Background(), key,
		&etcd.GetOptions{Recursive: true})
	var etcdOffsets = makeEtcdOffsetMap(key, offsetsRoot)

	var cdata consumerData
	cdata.members = make(map[string]memberData)
	cdata.owners = make(map[string]string)
	readHeads, writeHeads := getHeads(shardsRoot, &cdata)
	getJournalLag(etcdOffsets, writeHeads, readHeads, &cdata)
	return cdata
}

type MemberInfo struct {
	Journal journal.Name
	Member  string
	Master  bool
}

func makeEtcd(topic string) *etcd.Response {
	etcdClient, err := etcd.New(etcd.Config{
		Endpoints: []string{"http://" + *endpoints.EtcdEndpoint}})
	if err != nil {
		log.WithField("err", err).Fatal("failed to init etcd client")
	}
	var keysAPI = etcd.NewKeysAPI(etcdClient)
	var key = path.Join(topic, "items")
	shardsRoot, err := keysAPI.Get(context.Background(), key,
		&etcd.GetOptions{Recursive: true, Sort: true})
	if err != nil {
		log.WithFields(log.Fields{"err": err, "key": key}).Fatal("can't load etcd key")
	}
	return shardsRoot
}

func getMasterRepInfo(topic string) []MemberInfo {
	shardsRoot := makeEtcd(topic)

	var memberInfos []MemberInfo

	for _, node := range shardsRoot.Node.Nodes {
		var route = consensus.NewRoute(shardsRoot, node)
		var prefix = len(route.Item.Key) + 1

		// Derive journal name from item name.
		var parts = strings.Split(path.Base(route.Item.Key), "-")
		var journalName = fmt.Sprintf("pippio-journals/%s/part-%s",
			strings.Join(parts[1:len(parts)-1], "-"), parts[len(parts)-1])

		for i, member := range route.Entries {
			var memberID = member.Key[prefix:]

			// |memberID| should be an IP:port pair.
			if host, _, err := net.SplitHostPort(memberID); err != nil {
				log.WithFields(log.Fields{"master": memberID, "err": err}).Error(
					"route replica name is not a host/port pair")
			} else {
				memberInfos = append(memberInfos, MemberInfo{
					Member:  host,
					Master:  (i == 0),
					Journal: journal.Name(journalName),
				})
			}
		}
	}

	return memberInfos
}

func getHeads(shardsRoot *etcd.Response, cdata *consumerData) (map[string]int64, map[string]int64) {
	var readHeadOutput = make(chan journalHeadResult, 1024)
	var writeHeadOutput = make(chan journalHeadResult, 1024)
	var readHeadWg = new(sync.WaitGroup)
	var writeHeadWg = new(sync.WaitGroup)
	for _, node := range shardsRoot.Node.Nodes {
		var route = consensus.NewRoute(shardsRoot, node)
		var prefix = len(route.Item.Key) + 1

		// Derive journal name from item name.
		var parts = strings.Split(path.Base(route.Item.Key), "-")
		var journal = fmt.Sprintf("pippio-journals/%s/part-%s",
			strings.Join(parts[1:len(parts)-1], "-"), parts[len(parts)-1])

		cdata.journalNames = append(cdata.journalNames, journal)
		writeHeadWg.Add(1)
		go fetchWriteHead(journal, gazetteClient, writeHeadOutput, writeHeadWg)

		if len(route.Entries) == 0 {
			// Item has no master.
			log.WithField("item", route.Item.Key).Warn("no master for item")
			continue
		}

		for i, member := range route.Entries {
			var memberID = member.Key[prefix:]
			var info, ok = cdata.members[memberID]

			if !ok {
				cdata.memberNames = append(cdata.memberNames, memberID)
				info.states = make(map[string]string)
			}

			// Stores "ready" or "recovering" state.
			info.states[journal] = member.Value

			if i == 0 {
				info.masters++
				cdata.owners[journal] = memberID

				// |memberID| should be an IP:port pair.
				if host, _, err := net.SplitHostPort(memberID); err != nil {
					log.WithFields(log.Fields{"master": memberID, "err": err}).Error(
						"route replica name is not a host/port pair")
				} else {
					var debugURL = fmt.Sprintf("http://%s:8090/debug/vars", host)
					readHeadWg.Add(1)
					go fetchReadHead(journal, httpClient, debugURL, readHeadOutput, readHeadWg)
				}
			} else {
				info.replicas++
			}

			cdata.members[memberID] = info
		}
	}
	return collectHeads(readHeadWg, readHeadOutput, writeHeadWg, writeHeadOutput)
}

func collectHeads(readHeadWg *sync.WaitGroup, readHeadOutput chan journalHeadResult,
	writeHeadWg *sync.WaitGroup, writeHeadOutput chan journalHeadResult) (map[string]int64, map[string]int64) {
	var readHeads = make(map[string]int64)
	var writeHeads = make(map[string]int64)
	readHeadWg.Wait()
	close(readHeadOutput)
	for r := range readHeadOutput {
		if r.err != nil {
			log.WithFields(log.Fields{"err": r.err, "name": r.name}).Warn(
				"failed to retrieve read-head info")
		}
		readHeads[r.name] = r.head
	}
	writeHeadWg.Wait()
	close(writeHeadOutput)
	for r := range writeHeadOutput {
		if r.err != nil {
			log.WithFields(log.Fields{"err": r.err, "name": r.name}).Warn(
				"failed to retrieve write-head info")
		}
		writeHeads[r.name] = r.head
	}
	return readHeads, writeHeads
}

func getJournalLag(etcdOffsets, writeHeads, readHeads map[string]int64, cdata *consumerData) {
	// Determine total lag value per-member.
	var journalLag = make(map[string]journalData)
	for journal, writeHead := range writeHeads {
		var owner = cdata.owners[journal]
		var member = cdata.members[owner]
		var readHead int64
		var ok bool
		var state = "OK"
		var etcdState string

		if readHead, ok = readHeads[journal]; !ok {
			// |journalLag[journal]| remains unavailable.
		} else if etcdState, ok = member.states[journal]; ok && etcdState == "recovering" {
			// The shard reports that it is recovering. For now, use the read
			// offsets of the journal stored in Etcd, assuming that the replica
			// will recover to that point eventually (e.g. the lag cannot
			// be less than that amount.)
			state = "Recovering"
			readHead = etcdOffsets[journal]
		} else if readHead == journalNotBeingRead {
			// The shard is not recovering, but is not busy reading the
			// journal.
			state = "NotReading"
			readHead = etcdOffsets[journal]
		} else if readHead < etcdOffsets[journal] {
			// The shard is not recovering and is actively reading. The
			// offsets stored for this shard in Etcd exceed the ones reported
			// by the replica.
			state = "EtcdAhead"
			readHead = etcdOffsets[journal]
		}

		var delta = writeHead - readHead
		if delta >= 0 {
			journalLag[journal] = journalData{lag: delta, state: state}
			member.totalLag += delta
		} else {
			journalLag[journal] = journalData{lag: 0, state: state}
		}
		cdata.members[owner] = member
	}
	cdata.journalLag = journalLag
}

type journalHeadResult struct {
	name string
	head int64
	err  error
}

func fetchWriteHead(name string, client *gazette.Client, output chan<- journalHeadResult, wg *sync.WaitGroup) {
	var args = journal.ReadArgs{Journal: journal.Name(name)}
	var result, _ = client.Head(args)
	output <- journalHeadResult{name: name, err: result.Error, head: result.WriteHead}
	wg.Done()
}

func fetchReadHead(name string, client *http.Client, url string, output chan<- journalHeadResult, wg *sync.WaitGroup) {
	var vars varsData
	var result = journalHeadResult{name: name}

	// Connect to the debug port (8090) of the IP.
	var resp *http.Response
	resp, result.err = client.Get(url)

	if result.err != nil {
		log.WithFields(log.Fields{"url": url, "err": result.err}).Warn(
			"failed to access debug URL")
	} else if result.err = json.NewDecoder(resp.Body).Decode(&vars); result.err != nil {
		log.WithFields(log.Fields{"url": url, "err": result.err}).Error(
			"failed to decode debug URL")
	} else if reader, ok := vars.Gazette.Readers[name]; !ok {
		// The journal is owned by the member, but is not being read at this time.
		result.head = journalNotBeingRead
	} else {
		result.head = reader.Head
	}

	if resp != nil {
		resp.Body.Close()
	}
	output <- result
	wg.Done()
}

func expandPrefixList(prefixList []string) []string {
	var consumerList []string
	for i := 0; i < len(prefixList); i++ {
		var prefix = prefixList[i]
		var prefixNode, err = keysAPI.Get(context.Background(), prefix, nil)
		if err != nil {
			log.WithFields(log.Fields{"err": err, "key": prefix}).Fatal("could not get Etcd key")
		}

		var isConsumer bool
		for _, child := range prefixNode.Node.Nodes {
			if base := path.Base(child.Key); base == "items" || base == "members" {
				// Path is a consumer root.
				consumerList = append(consumerList, prefix)
				isConsumer = true
				break
			}
		}
		if !isConsumer {
			for _, child := range prefixNode.Node.Nodes {
				// Path is a directory that contains consumer roots. Check each
				// one individually.
				prefixList = append(prefixList, child.Key)
			}
		}
	}

	return consumerList
}

func makeEtcdOffsetMap(key string, r *etcd.Response) map[string]int64 {
	var offsets = make(map[string]int64)
	if r == nil {
		return offsets
	}

	var visit = r.Node.Nodes
	for i := 0; i < len(visit); i++ {
		var node = visit[i]
		if node.Dir {
			visit = append(visit, node.Nodes...)
		} else {
			var off, err = strconv.ParseInt(node.Value, 16, 64)
			if err != nil {
				log.WithFields(log.Fields{"key": key, "err": err}).Warn("can't parse offset")
			} else {
				offsets[strings.TrimPrefix(node.Key, key+"/")] = off
			}
		}
	}
	return offsets
}

type prefixFlagSet []string

func (f *prefixFlagSet) String() string {
	return strings.Join(*f, ",")
}

func (f *prefixFlagSet) Set(prefix string) error {
	*f = append(*f, prefix)
	return nil
}
