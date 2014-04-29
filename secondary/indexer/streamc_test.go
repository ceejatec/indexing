package indexer

import (
	"fmt"
	c "github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/protobuf"
	"io/ioutil"
	"log"
	"testing"
)

var addr = "localhost:8888"

func TestStreamClient(t *testing.T) {
	maxconns, maxvbuckets, mutChanSize := 2, 8, 100
	log.SetOutput(ioutil.Discard)

	// start server
	msgch := make(chan interface{}, mutChanSize)
	errch := make(chan interface{}, 1000)
	daemon := doServer(addr, t, msgch, errch, 100)
	flags := StreamTransportFlag(0).SetProtobuf()

	// start client and test number of connection.
	client, err := NewStreamClient(addr, maxconns, flags)
	if err != nil {
		t.Fatal(err)
	} else if len(client.conns) != maxconns {
		t.Fatal("failed stream client connections")
	} else if len(client.conns) != len(client.connChans) {
		t.Fatal("failed stream client connection channels")
	} else if len(client.conns) != len(client.conn2Vbs) {
		t.Fatal("failed stream client connection channels")
	} else {
		maxBuckets := 2
		vbmaps := makeVbmaps(maxvbuckets, maxBuckets) // vbmaps
		for i := 0; i < maxBuckets; i++ {
			if err := client.SendVbmap(vbmaps[i]); err != nil {
				t.Fatal(err)
			}
		}
		validateClientInstance(client, maxvbuckets, maxconns, maxBuckets, t)
	}
	client.Close()
	daemon.Close()
}

func TestStreamClientBegin(t *testing.T) {
	maxconns, maxvbuckets, mutChanSize := 2, 8, 100
	log.SetOutput(ioutil.Discard)

	// start server
	msgch := make(chan interface{}, mutChanSize)
	errch := make(chan interface{}, 1000)
	daemon := doServer(addr, t, msgch, errch, 100)
	flags := StreamTransportFlag(0).SetProtobuf()

	// start client
	client, _ := NewStreamClient(addr, maxconns, flags)
	maxBuckets := 2
	vbmaps := makeVbmaps(maxvbuckets, maxBuckets) // vbmaps
	for i := 0; i < maxBuckets; i++ {
		if err := client.SendVbmap(vbmaps[i]); err != nil {
			t.Fatal(err)
		}
	}
	// test a live StreamBegin
	bucket, vbno, vbuuid := "default0", uint16(maxvbuckets), uint64(1111)
	uuid := c.ID(bucket, vbno)
	vals, _ := client.Getcontext()
	vbChans := vals[0].(map[string]chan interface{})
	if _, ok := vbChans[uuid]; ok {
		t.Fatal("duplicate id")
	}
	vb := c.NewVbKeyVersions(bucket, vbno, vbuuid, 1)
	seqno, docid, maxCount := uint64(10), []byte("document-name"), 10
	kv := c.NewKeyVersions(seqno, docid, maxCount)
	kv.AddStreamBegin()
	vb.AddKeyVersions(kv)
	err := client.SendKeyVersions([]*c.VbKeyVersions{vb})
	client.Getcontext() // syncup
	if err != nil {
		t.Fatal(err)
	} else if _, ok := vbChans[uuid]; !ok {
		fmt.Printf("%v %v\n", len(vbChans), uuid)
		t.Fatal("failed StreamBegin")
	}
	client.Close()
	daemon.Close()
}

func TestStreamClientEnd(t *testing.T) {
	maxconns, maxvbuckets, mutChanSize := 2, 8, 100
	log.SetOutput(ioutil.Discard)

	// start server
	msgch := make(chan interface{}, mutChanSize)
	errch := make(chan interface{}, 1000)
	daemon := doServer(addr, t, msgch, errch, 100)
	flags := StreamTransportFlag(0).SetProtobuf()

	// start client
	client, _ := NewStreamClient(addr, maxconns, flags)
	maxBuckets := 2
	vbmaps := makeVbmaps(maxvbuckets, maxBuckets) // vbmaps
	for i := 0; i < maxBuckets; i++ {
		if err := client.SendVbmap(vbmaps[i]); err != nil {
			t.Fatal(err)
		}
	}
	// test a live StreamEnd
	bucket, vbno := "default0", vbmaps[0].Vbuckets[0]
	vbuuid := uint64(vbmaps[0].Vbuuids[0])
	uuid := c.ID(bucket, vbno)
	vals, _ := client.Getcontext()
	vbChans := vals[0].(map[string]chan interface{})
	if _, ok := vbChans[uuid]; !ok {
		t.Fatal("expected uuid")
	}
	vb := c.NewVbKeyVersions(bucket, vbno, vbuuid, 1)
	seqno, docid, maxCount := uint64(10), []byte("document-name"), 10
	kv := c.NewKeyVersions(seqno, docid, maxCount)
	kv.AddStreamEnd()
	vb.AddKeyVersions(kv)
	err := client.SendKeyVersions([]*c.VbKeyVersions{vb})
	client.Getcontext() // syncup
	if err != nil {
		t.Fatal(err)
	} else if _, ok := vbChans[uuid]; ok {
		t.Fatal("failed StreamEnd")
	}
	client.Close()
	daemon.Close()
}

func doServer(addr string, tb testing.TB, msgch, errch chan interface{}, mutChanSize int) *MutationStream {
	var mStream *MutationStream
	var err error

	mutch := make(chan []*protobuf.VbKeyVersions, mutChanSize)
	sbch := make(chan interface{}, 100)
	if mStream, err = NewMutationStream(addr, mutch, sbch); err != nil {
		tb.Fatal(err)
	}

	go func() {
		var mutn, err interface{}
		var ok bool
		for {
			select {
			case mutn, ok = <-mutch:
				msgch <- mutn
			case err, ok = <-sbch:
				errch <- err
			}
			if ok == false {
				return
			}
		}
	}()
	return mStream
}

func makeVbmaps(maxvbuckets int, maxBuckets int) []*c.VbConnectionMap {
	vbmaps := make([]*c.VbConnectionMap, 0, maxBuckets)
	for i := 0; i < maxBuckets; i++ {
		vbmap := &c.VbConnectionMap{
			Bucket:   fmt.Sprintf("default%v", i),
			Vbuckets: make([]uint16, 0, maxvbuckets),
			Vbuuids:  make([]uint64, 0, maxvbuckets),
		}
		for i := 0; i < maxvbuckets; i++ {
			vbmap.Vbuckets = append(vbmap.Vbuckets, uint16(i))
			vbmap.Vbuuids = append(vbmap.Vbuuids, uint64(i*10))
		}
		vbmaps = append(vbmaps, vbmap)
	}
	return vbmaps
}

func verify(msgch, errch chan interface{}, fn func(mutn, err interface{})) {
	select {
	case mutn := <-msgch:
		fn(mutn, nil)
	case err := <-errch:
		fn(nil, err)
	}
}

func validateClientInstance(
	client *StreamClient, maxvbuckets, maxconns, maxBuckets int, t *testing.T) {

	ref := ((maxvbuckets / maxconns) * maxBuckets)
	vals, _ := client.Getcontext()
	vbChans := vals[0].(map[string]chan interface{})
	// validate vbucket channels
	refChans := make(map[chan interface{}][]string)
	for uuid, ch := range vbChans {
		if ids, ok := refChans[ch]; !ok {
			refChans[ch] = make([]string, 0)
		} else {
			refChans[ch] = append(ids, uuid)
		}
	}
	// validate connection to vbuckets.
	conn2Vbs := vals[1].(map[int][]string)
	for _, ids := range conn2Vbs {
		if len(ids) != ref {
			t.Fatal("failed stream client, vbucket mapping")
		}
	}
}