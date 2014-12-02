// Package queryport provides a simple library to spawn a queryport and access
// queryport via passive client API.
//
// ---> Request                 ---> Request
//      <--- Response                <--- Response
//      <--- Response                <--- Response
//      ...                     ---> EndStreamRequest
//      <--- StreamEndResponse       <--- Response (residue)
//                                   <--- StreamEndResponse

package client

import "errors"
import "fmt"
import "io"
import "net"
import "time"

import "github.com/couchbase/indexing/secondary/common"
import protobuf "github.com/couchbase/indexing/secondary/protobuf/query"
import "github.com/couchbase/indexing/secondary/transport"
import "github.com/couchbaselabs/goprotobuf/proto"

// ErrorProtocol
var ErrorProtocol = errors.New("queryport.protocol")

// ResponseHandler shall interpret response packets from server
// and handle them. If handler is not interested in receiving any
// more response it shall return false, else it shall continue
// until *protobufEncode.StreamEndResponse message is received.
type ResponseHandler func(resp interface{}) bool

// Client structure.
type Client struct {
	raddr string
	pool  *connectionPool
	// config params
	maxPayload         int // TODO: what if it exceeds ?
	readDeadline       time.Duration
	writeDeadline      time.Duration
	poolSize           int
	poolOverflow       int
	cpTimeout          time.Duration
	cpAvailWaitTimeout time.Duration
	logPrefix          string
}

// NewClient instance with `raddr` pointing to queryport server.
func NewClient(raddr string, config common.Config) (c *Client) {
	t := time.Duration(config["connPoolAvailWaitTimeout"].Int())
	c = &Client{
		raddr:              raddr,
		maxPayload:         config["maxPayload"].Int(),
		readDeadline:       time.Duration(config["readDeadline"].Int()),
		writeDeadline:      time.Duration(config["writeDeadline"].Int()),
		poolSize:           config["poolSize"].Int(),
		poolOverflow:       config["poolOverflow"].Int(),
		cpTimeout:          time.Duration(config["connPoolTimeout"].Int()),
		cpAvailWaitTimeout: t,
		logPrefix:          fmt.Sprintf("[QueryPortClient:%q]", raddr),
	}
	c.pool = newConnectionPool(
		raddr, c.poolSize, c.poolOverflow, c.maxPayload, c.cpTimeout,
		c.cpAvailWaitTimeout)
	common.Infof("%v started ...\n", c.logPrefix)
	return c
}

// Close the client and all open connections with server.
func (c *Client) Close() {
	c.pool.Close()
	common.Infof("%v ... stopped\n", c.logPrefix)
}

// Statistics for index range.
func (c *Client) Statistics(
	index, bucket string, low, high []byte, equal [][]byte,
	inclusion uint32) (*protobuf.IndexStatistics, error) {

	connectn, err := c.pool.Get()
	if err != nil {
		return nil, err
	}
	healthy := true
	defer c.pool.Return(connectn, healthy)

	conn, pkt := connectn.conn, connectn.pkt

	r := &protobuf.Range{
		Low:       low,
		High:      high,
		Inclusion: proto.Uint32(inclusion),
	}

	req := &protobuf.StatisticsRequest{
		Span: &protobuf.Span{
			Range: r,
			Equal: equal,
		},
		IndexName: proto.String(index),
		Bucket:    proto.String(bucket),
	}
	// ---> protobuf.StatisticsRequest
	if err := c.sendRequest(conn, pkt, req); err != nil {
		msg := "%v Statistics() request transport failed `%v`\n"
		common.Errorf(msg, c.logPrefix, err)
		healthy = false
		return nil, err
	}

	timeoutMs := c.readDeadline * time.Millisecond
	conn.SetReadDeadline(time.Now().Add(timeoutMs))
	// <--- protobuf.StatisticsResponse
	resp, err := pkt.Receive(conn)
	if err != nil {
		msg := "%v Statistics() response transport failed `%v`\n"
		common.Errorf(msg, c.logPrefix, err)
		healthy = false
		return nil, err
	}

	conn.SetReadDeadline(time.Now().Add(timeoutMs))
	// <--- protobuf.StreamEndResponse (skipped)
	endResp, err := pkt.Receive(conn)
	if _, ok := endResp.(*protobuf.StreamEndResponse); !ok {
		return nil, ErrorProtocol
	}

	statResp := resp.(*protobuf.StatisticsResponse)
	if statResp.GetErr() != nil {
		err = errors.New(statResp.GetErr().GetError())
		return nil, err
	}

	return statResp.GetStats(), nil
}

// Scan index for a range.
func (c *Client) Scan(
	index, bucket string, low, high []byte, equal [][]byte, inclusion uint32,
	pageSize int64, distinct bool, limit int64, callb ResponseHandler) error {

	connectn, err := c.pool.Get()
	if err != nil {
		return err
	}
	healthy := true
	defer c.pool.Return(connectn, healthy)

	conn, pkt := connectn.conn, connectn.pkt

	incl := proto.Uint32(inclusion)
	r := &protobuf.Range{Low: low, High: high, Inclusion: incl}
	req := &protobuf.ScanRequest{
		Span:      &protobuf.Span{Range: r, Equal: equal},
		Distinct:  proto.Bool(distinct),
		PageSize:  proto.Int64(pageSize),
		Limit:     proto.Int64(limit),
		IndexName: proto.String(index),
		Bucket:    proto.String(bucket),
	}
	// ---> protobuf.ScanRequest
	if err := c.sendRequest(conn, pkt, req); err != nil {
		msg := "%v Scan() request transport failed `%v`\n"
		common.Errorf(msg, c.logPrefix, err)
		healthy = false
		return err
	}

	cont := true
	for cont {
		// <--- protobuf.ResponseStream
		cont, healthy, err = c.streamResponse(conn, pkt, callb)
		if err != nil {
			msg := "%v Scan() response failed `%v`\n"
			common.Errorf(msg, c.logPrefix, err)
		}
	}
	return nil
}

// ScanAll for full table scan.
func (c *Client) ScanAll(index, bucket string, pageSize int64, limit int64,
	callb func(interface{}) bool) error {

	connectn, err := c.pool.Get()
	if err != nil {
		return err
	}
	healthy := true
	defer c.pool.Return(connectn, healthy)

	conn, pkt := connectn.conn, connectn.pkt

	req := &protobuf.ScanAllRequest{
		PageSize:  proto.Int64(pageSize),
		Limit:     proto.Int64(limit),
		IndexName: proto.String(index),
		Bucket:    proto.String(bucket),
	}
	if err := c.sendRequest(conn, pkt, req); err != nil {
		common.Errorf(
			"%v ScanAll() request transport failed `%v`\n",
			c.logPrefix, err)
		healthy = false
		return err
	}

	cont := true
	for cont {
		cont, healthy, err = c.streamResponse(conn, pkt, callb)
		if err != nil {
			msg := "%v ScanAll() response failed `%v`\n"
			common.Errorf(msg, c.logPrefix, err)
		}
	}
	return nil
}

func (c *Client) sendRequest(
	conn net.Conn, pkt *transport.TransportPacket, req interface{}) (err error) {

	timeoutMs := c.writeDeadline * time.Millisecond
	conn.SetWriteDeadline(time.Now().Add(timeoutMs))
	return pkt.Send(conn, req)
}

func (c *Client) streamResponse(
	conn net.Conn,
	pkt *transport.TransportPacket,
	callb ResponseHandler) (cont bool, healthy bool, err error) {

	var resp interface{}
	var finish bool

	laddr := conn.LocalAddr()
	timeoutMs := c.readDeadline * time.Millisecond
	conn.SetReadDeadline(time.Now().Add(timeoutMs))
	resp, err = pkt.Receive(conn)
	if err != nil {
		callb(err) // callback with error
		cont, healthy = false, false
		if err != io.EOF {
			msg := "%v connection %q response transport failed `%v`\n"
			common.Errorf(msg, c.logPrefix, laddr, err)
		}

	} else if _, finish = resp.(*protobuf.StreamEndResponse); finish {
		msg := "%v connection %q received StreamEndResponse"
		common.Debugf(msg, c.logPrefix, laddr)
		callb(resp) // callback most likely return true
		cont, healthy = false, true

	} else {
		cont = callb(resp)
		healthy = true
	}

	if cont == false && healthy == true && finish == false {
		err = c.closeStream(conn, pkt)
	}
	return
}

func (c *Client) closeStream(
	conn net.Conn, pkt *transport.TransportPacket) (err error) {

	var resp interface{}
	laddr := conn.LocalAddr()
	// request server to end the stream.
	err = c.sendRequest(conn, pkt, &protobuf.EndStreamRequest{})
	if err != nil {
		msg := "%v closeStream() request transport failed `%v`\n"
		common.Errorf(msg, c.logPrefix, err)
		return
	}
	msg := "%v connection %q transmitted protobuf.EndStreamRequest"
	common.Debugf(msg, c.logPrefix, laddr)

	timeoutMs := c.readDeadline * time.Millisecond
	// flush the connection until stream has ended.
	for true {
		conn.SetReadDeadline(time.Now().Add(timeoutMs))
		resp, err = pkt.Receive(conn)
		if err == io.EOF {
			common.Errorf("%v connection %q closed \n", c.logPrefix, laddr)
			return

		} else if err != nil {
			msg := "%v connection %q response transport failed `%v`\n"
			common.Errorf(msg, c.logPrefix, laddr, err)
			return

		} else if _, ok := resp.(*protobuf.StreamEndResponse); ok {
			return
		}
	}
	return
}