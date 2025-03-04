// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Copyright 2013 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// The MIT License (MIT)
//
// Copyright (c) 2014 wandoulabs
// Copyright (c) 2014 siddontang
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	goerr "errors"
	"fmt"
	"io"
	"net"
	"os/user"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/errno"
	"github.com/pingcap/tidb/executor"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/parser"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/auth"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/parser/terror"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/plugin"
	"github.com/pingcap/tidb/privilege"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	storeerr "github.com/pingcap/tidb/store/driver/error"
	"github.com/pingcap/tidb/tablecodec"
	tidbutil "github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/arena"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/execdetails"
	"github.com/pingcap/tidb/util/hack"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/memory"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tikv/client-go/v2/util"
	"go.uber.org/zap"
)

const (
	connStatusDispatching int32 = iota
	connStatusReading
	connStatusShutdown     // Closed by server.
	connStatusWaitShutdown // Notified by server to close.
)

var (
	queryTotalCountOk = [...]prometheus.Counter{
		mysql.ComSleep:            metrics.QueryTotalCounter.WithLabelValues("Sleep", "OK"),
		mysql.ComQuit:             metrics.QueryTotalCounter.WithLabelValues("Quit", "OK"),
		mysql.ComInitDB:           metrics.QueryTotalCounter.WithLabelValues("InitDB", "OK"),
		mysql.ComQuery:            metrics.QueryTotalCounter.WithLabelValues("Query", "OK"),
		mysql.ComPing:             metrics.QueryTotalCounter.WithLabelValues("Ping", "OK"),
		mysql.ComFieldList:        metrics.QueryTotalCounter.WithLabelValues("FieldList", "OK"),
		mysql.ComStmtPrepare:      metrics.QueryTotalCounter.WithLabelValues("StmtPrepare", "OK"),
		mysql.ComStmtExecute:      metrics.QueryTotalCounter.WithLabelValues("StmtExecute", "OK"),
		mysql.ComStmtFetch:        metrics.QueryTotalCounter.WithLabelValues("StmtFetch", "OK"),
		mysql.ComStmtClose:        metrics.QueryTotalCounter.WithLabelValues("StmtClose", "OK"),
		mysql.ComStmtSendLongData: metrics.QueryTotalCounter.WithLabelValues("StmtSendLongData", "OK"),
		mysql.ComStmtReset:        metrics.QueryTotalCounter.WithLabelValues("StmtReset", "OK"),
		mysql.ComSetOption:        metrics.QueryTotalCounter.WithLabelValues("SetOption", "OK"),
	}
	queryTotalCountErr = [...]prometheus.Counter{
		mysql.ComSleep:            metrics.QueryTotalCounter.WithLabelValues("Sleep", "Error"),
		mysql.ComQuit:             metrics.QueryTotalCounter.WithLabelValues("Quit", "Error"),
		mysql.ComInitDB:           metrics.QueryTotalCounter.WithLabelValues("InitDB", "Error"),
		mysql.ComQuery:            metrics.QueryTotalCounter.WithLabelValues("Query", "Error"),
		mysql.ComPing:             metrics.QueryTotalCounter.WithLabelValues("Ping", "Error"),
		mysql.ComFieldList:        metrics.QueryTotalCounter.WithLabelValues("FieldList", "Error"),
		mysql.ComStmtPrepare:      metrics.QueryTotalCounter.WithLabelValues("StmtPrepare", "Error"),
		mysql.ComStmtExecute:      metrics.QueryTotalCounter.WithLabelValues("StmtExecute", "Error"),
		mysql.ComStmtFetch:        metrics.QueryTotalCounter.WithLabelValues("StmtFetch", "Error"),
		mysql.ComStmtClose:        metrics.QueryTotalCounter.WithLabelValues("StmtClose", "Error"),
		mysql.ComStmtSendLongData: metrics.QueryTotalCounter.WithLabelValues("StmtSendLongData", "Error"),
		mysql.ComStmtReset:        metrics.QueryTotalCounter.WithLabelValues("StmtReset", "Error"),
		mysql.ComSetOption:        metrics.QueryTotalCounter.WithLabelValues("SetOption", "Error"),
	}

	queryDurationHistogramUse      = metrics.QueryDurationHistogram.WithLabelValues("Use")
	queryDurationHistogramShow     = metrics.QueryDurationHistogram.WithLabelValues("Show")
	queryDurationHistogramBegin    = metrics.QueryDurationHistogram.WithLabelValues("Begin")
	queryDurationHistogramCommit   = metrics.QueryDurationHistogram.WithLabelValues("Commit")
	queryDurationHistogramRollback = metrics.QueryDurationHistogram.WithLabelValues("Rollback")
	queryDurationHistogramInsert   = metrics.QueryDurationHistogram.WithLabelValues("Insert")
	queryDurationHistogramReplace  = metrics.QueryDurationHistogram.WithLabelValues("Replace")
	queryDurationHistogramDelete   = metrics.QueryDurationHistogram.WithLabelValues("Delete")
	queryDurationHistogramUpdate   = metrics.QueryDurationHistogram.WithLabelValues("Update")
	queryDurationHistogramSelect   = metrics.QueryDurationHistogram.WithLabelValues("Select")
	queryDurationHistogramExecute  = metrics.QueryDurationHistogram.WithLabelValues("Execute")
	queryDurationHistogramSet      = metrics.QueryDurationHistogram.WithLabelValues("Set")
	queryDurationHistogramGeneral  = metrics.QueryDurationHistogram.WithLabelValues(metrics.LblGeneral)

	disconnectNormal            = metrics.DisconnectionCounter.WithLabelValues(metrics.LblOK)
	disconnectByClientWithError = metrics.DisconnectionCounter.WithLabelValues(metrics.LblError)
	disconnectErrorUndetermined = metrics.DisconnectionCounter.WithLabelValues("undetermined")

	connIdleDurationHistogramNotInTxn = metrics.ConnIdleDurationHistogram.WithLabelValues("0")
	connIdleDurationHistogramInTxn    = metrics.ConnIdleDurationHistogram.WithLabelValues("1")
)

// newClientConn creates a *clientConn object.
func newClientConn(s *Server) *clientConn {
	return &clientConn{
		server:       s,
		connectionID: s.globalConnID.NextID(),
		collation:    mysql.DefaultCollationID,
		alloc:        arena.NewAllocator(32 * 1024),
		chunkAlloc:   chunk.NewAllocator(),
		status:       connStatusDispatching,
		lastActive:   time.Now(),
		authPlugin:   mysql.AuthNativePassword,
	}
}

// clientConn represents a connection between server and client, it maintains connection specific state,
// handles client query.
type clientConn struct {
	pkt           *packetIO         // a helper to read and write data in packet format.
	bufReadConn   *bufferedReadConn // a buffered-read net.Conn or buffered-read tls.Conn.
	tlsConn       *tls.Conn         // TLS connection, nil if not TLS.
	server        *Server           // a reference of server instance.
	capability    uint32            // client capability affects the way server handles client request.
	connectionID  uint64            // atomically allocated by a global variable, unique in process scope.
	user          string            // user of the client.
	dbname        string            // default database name.
	salt          []byte            // random bytes used for authentication.
	alloc         arena.Allocator   // an memory allocator for reducing memory allocation.
	chunkAlloc    chunk.Allocator
	lastPacket    []byte            // latest sql query string, currently used for logging error.
	ctx           *TiDBContext      // an interface to execute sql statements.
	attrs         map[string]string // attributes parsed from client handshake response, not used for now.
	peerHost      string            // peer host
	peerPort      string            // peer port
	status        int32             // dispatching/reading/shutdown/waitshutdown
	lastCode      uint16            // last error code
	collation     uint8             // collation used by client, may be different from the collation used by database.
	lastActive    time.Time         // last active time
	authPlugin    string            // default authentication plugin
	isUnixSocket  bool              // connection is Unix Socket file
	rsEncoder     *resultEncoder    // rsEncoder is used to encode the string result to different charsets.
	socketCredUID uint32            // UID from the other end of the Unix Socket
	// mu is used for cancelling the execution of current transaction.
	mu struct {
		sync.RWMutex
		cancelFunc context.CancelFunc
	}
}

func (cc *clientConn) String() string {
	collationStr := mysql.Collations[cc.collation]
	return fmt.Sprintf("id:%d, addr:%s status:%b, collation:%s, user:%s",
		cc.connectionID, cc.bufReadConn.RemoteAddr(), cc.ctx.Status(), collationStr, cc.user,
	)
}

// authSwitchRequest is used by the server to ask the client to switch to a different authentication
// plugin. MySQL 8.0 libmysqlclient based clients by default always try `caching_sha2_password`, even
// when the server advertises the its default to be `mysql_native_password`. In addition to this switching
// may be needed on a per user basis as the authentication method is set per user.
// https://dev.mysql.com/doc/internals/en/connection-phase-packets.html#packet-Protocol::AuthSwitchRequest
// https://bugs.mysql.com/bug.php?id=93044
func (cc *clientConn) authSwitchRequest(ctx context.Context, plugin string) ([]byte, error) {
	enclen := 1 + len(plugin) + 1 + len(cc.salt) + 1
	data := cc.alloc.AllocWithLen(4, enclen)
	data = append(data, mysql.AuthSwitchRequest) // switch request
	data = append(data, []byte(plugin)...)
	data = append(data, byte(0x00)) // requires null
	data = append(data, cc.salt...)
	data = append(data, 0)
	err := cc.writePacket(data)
	if err != nil {
		logutil.Logger(ctx).Debug("write response to client failed", zap.Error(err))
		return nil, err
	}
	err = cc.flush(ctx)
	if err != nil {
		logutil.Logger(ctx).Debug("flush response to client failed", zap.Error(err))
		return nil, err
	}
	resp, err := cc.readPacket()
	if err != nil {
		err = errors.SuspendStack(err)
		if errors.Cause(err) == io.EOF {
			logutil.Logger(ctx).Warn("authSwitchRequest response fail due to connection has be closed by client-side")
		} else {
			logutil.Logger(ctx).Warn("authSwitchRequest response fail", zap.Error(err))
		}
		return nil, err
	}
	cc.authPlugin = plugin
	return resp, nil
}

// handshake works like TCP handshake, but in a higher level, it first writes initial packet to client,
// during handshake, client and server negotiate compatible features and do authentication.
// After handshake, client can send sql query to server.
func (cc *clientConn) handshake(ctx context.Context) error {
	if err := cc.writeInitialHandshake(ctx); err != nil {
		if errors.Cause(err) == io.EOF {
			logutil.Logger(ctx).Debug("Could not send handshake due to connection has be closed by client-side")
		} else {
			logutil.Logger(ctx).Debug("Write init handshake to client fail", zap.Error(errors.SuspendStack(err)))
		}
		return err
	}
	if err := cc.readOptionalSSLRequestAndHandshakeResponse(ctx); err != nil {
		err1 := cc.writeError(ctx, err)
		if err1 != nil {
			logutil.Logger(ctx).Debug("writeError failed", zap.Error(err1))
		}
		return err
	}

	// MySQL supports an "init_connect" query, which can be run on initial connection.
	// The query must return a non-error or the client is disconnected.
	if err := cc.initConnect(ctx); err != nil {
		logutil.Logger(ctx).Warn("init_connect failed", zap.Error(err))
		initErr := errNewAbortingConnection.FastGenByArgs(cc.connectionID, "unconnected", cc.user, cc.peerHost, "init_connect command failed")
		if err1 := cc.writeError(ctx, initErr); err1 != nil {
			terror.Log(err1)
		}
		return initErr
	}

	data := cc.alloc.AllocWithLen(4, 32)
	data = append(data, mysql.OKHeader)
	data = append(data, 0, 0)
	if cc.capability&mysql.ClientProtocol41 > 0 {
		data = dumpUint16(data, mysql.ServerStatusAutocommit)
		data = append(data, 0, 0)
	}

	err := cc.writePacket(data)
	cc.pkt.sequence = 0
	if err != nil {
		err = errors.SuspendStack(err)
		logutil.Logger(ctx).Debug("write response to client failed", zap.Error(err))
		return err
	}

	err = cc.flush(ctx)
	if err != nil {
		err = errors.SuspendStack(err)
		logutil.Logger(ctx).Debug("flush response to client failed", zap.Error(err))
		return err
	}
	return err
}

func (cc *clientConn) Close() error {
	cc.server.rwlock.Lock()
	delete(cc.server.clients, cc.connectionID)
	connections := len(cc.server.clients)
	cc.server.rwlock.Unlock()
	return closeConn(cc, connections)
}

func closeConn(cc *clientConn, connections int) error {
	metrics.ConnGauge.Set(float64(connections))
	err := cc.bufReadConn.Close()
	terror.Log(err)
	if cc.ctx != nil {
		return cc.ctx.Close()
	}
	return nil
}

func (cc *clientConn) closeWithoutLock() error {
	delete(cc.server.clients, cc.connectionID)
	return closeConn(cc, len(cc.server.clients))
}

// writeInitialHandshake sends server version, connection ID, server capability, collation, server status
// and auth salt to the client.
func (cc *clientConn) writeInitialHandshake(ctx context.Context) error {
	data := make([]byte, 4, 128)

	// min version 10
	data = append(data, 10)
	// server version[00]
	data = append(data, mysql.ServerVersion...)
	data = append(data, 0)
	// connection id
	data = append(data, byte(cc.connectionID), byte(cc.connectionID>>8), byte(cc.connectionID>>16), byte(cc.connectionID>>24))
	// auth-plugin-data-part-1
	data = append(data, cc.salt[0:8]...)
	// filler [00]
	data = append(data, 0)
	// capability flag lower 2 bytes, using default capability here
	data = append(data, byte(cc.server.capability), byte(cc.server.capability>>8))
	// charset
	if cc.collation == 0 {
		cc.collation = uint8(mysql.DefaultCollationID)
	}
	data = append(data, cc.collation)
	// status
	data = dumpUint16(data, mysql.ServerStatusAutocommit)
	// below 13 byte may not be used
	// capability flag upper 2 bytes, using default capability here
	data = append(data, byte(cc.server.capability>>16), byte(cc.server.capability>>24))
	// length of auth-plugin-data
	data = append(data, byte(len(cc.salt)+1))
	// reserved 10 [00]
	data = append(data, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	// auth-plugin-data-part-2
	data = append(data, cc.salt[8:]...)
	data = append(data, 0)
	// auth-plugin name
	if cc.ctx == nil {
		if err := cc.openSession(); err != nil {
			return err
		}
	}
	defAuthPlugin, err := variable.GetGlobalSystemVar(cc.ctx.GetSessionVars(), variable.DefaultAuthPlugin)
	if err != nil {
		return err
	}
	cc.authPlugin = defAuthPlugin
	data = append(data, []byte(defAuthPlugin)...)

	// Close the session to force this to be re-opened after we parse the response. This is needed
	// to ensure we use the collation and client flags from the response for the session.
	if err = cc.ctx.Close(); err != nil {
		return err
	}
	cc.ctx = nil

	data = append(data, 0)
	if err = cc.writePacket(data); err != nil {
		return err
	}
	return cc.flush(ctx)
}

func (cc *clientConn) readPacket() ([]byte, error) {
	return cc.pkt.readPacket()
}

func (cc *clientConn) writePacket(data []byte) error {
	failpoint.Inject("FakeClientConn", func() {
		if cc.pkt == nil {
			failpoint.Return(nil)
		}
	})
	return cc.pkt.writePacket(data)
}

// getSessionVarsWaitTimeout get session variable wait_timeout
func (cc *clientConn) getSessionVarsWaitTimeout(ctx context.Context) uint64 {
	valStr, exists := cc.ctx.GetSessionVars().GetSystemVar(variable.WaitTimeout)
	if !exists {
		return variable.DefWaitTimeout
	}
	waitTimeout, err := strconv.ParseUint(valStr, 10, 64)
	if err != nil {
		logutil.Logger(ctx).Warn("get sysval wait_timeout failed, use default value", zap.Error(err))
		// if get waitTimeout error, use default value
		return variable.DefWaitTimeout
	}
	return waitTimeout
}

type handshakeResponse41 struct {
	Capability uint32
	Collation  uint8
	User       string
	DBName     string
	Auth       []byte
	AuthPlugin string
	Attrs      map[string]string
}

// parseOldHandshakeResponseHeader parses the old version handshake header HandshakeResponse320
func parseOldHandshakeResponseHeader(ctx context.Context, packet *handshakeResponse41, data []byte) (parsedBytes int, err error) {
	// Ensure there are enough data to read:
	// https://dev.mysql.com/doc/internals/en/connection-phase-packets.html#packet-Protocol::HandshakeResponse320
	logutil.Logger(ctx).Debug("try to parse hanshake response as Protocol::HandshakeResponse320", zap.ByteString("packetData", data))
	if len(data) < 2+3 {
		logutil.Logger(ctx).Error("got malformed handshake response", zap.ByteString("packetData", data))
		return 0, mysql.ErrMalformPacket
	}
	offset := 0
	// capability
	capability := binary.LittleEndian.Uint16(data[:2])
	packet.Capability = uint32(capability)

	// be compatible with Protocol::HandshakeResponse41
	packet.Capability |= mysql.ClientProtocol41

	offset += 2
	// skip max packet size
	offset += 3
	// usa default CharsetID
	packet.Collation = mysql.CollationNames["utf8mb4_general_ci"]

	return offset, nil
}

// parseOldHandshakeResponseBody parse the HandshakeResponse for Protocol::HandshakeResponse320 (except the common header part).
func parseOldHandshakeResponseBody(ctx context.Context, packet *handshakeResponse41, data []byte, offset int) (err error) {
	defer func() {
		// Check malformat packet cause out of range is disgusting, but don't panic!
		if r := recover(); r != nil {
			logutil.Logger(ctx).Error("handshake panic", zap.ByteString("packetData", data), zap.Stack("stack"))
			err = mysql.ErrMalformPacket
		}
	}()
	// user name
	packet.User = string(data[offset : offset+bytes.IndexByte(data[offset:], 0)])
	offset += len(packet.User) + 1

	if packet.Capability&mysql.ClientConnectWithDB > 0 {
		if len(data[offset:]) > 0 {
			idx := bytes.IndexByte(data[offset:], 0)
			packet.DBName = string(data[offset : offset+idx])
			offset = offset + idx + 1
		}
		if len(data[offset:]) > 0 {
			packet.Auth = data[offset : offset+bytes.IndexByte(data[offset:], 0)]
		}
	} else {
		packet.Auth = data[offset : offset+bytes.IndexByte(data[offset:], 0)]
	}

	return nil
}

// parseHandshakeResponseHeader parses the common header of SSLRequest and HandshakeResponse41.
func parseHandshakeResponseHeader(ctx context.Context, packet *handshakeResponse41, data []byte) (parsedBytes int, err error) {
	// Ensure there are enough data to read:
	// http://dev.mysql.com/doc/internals/en/connection-phase-packets.html#packet-Protocol::SSLRequest
	if len(data) < 4+4+1+23 {
		logutil.Logger(ctx).Error("got malformed handshake response", zap.ByteString("packetData", data))
		return 0, mysql.ErrMalformPacket
	}

	offset := 0
	// capability
	capability := binary.LittleEndian.Uint32(data[:4])
	packet.Capability = capability
	offset += 4
	// skip max packet size
	offset += 4
	// charset, skip, if you want to use another charset, use set names
	packet.Collation = data[offset]
	offset++
	// skip reserved 23[00]
	offset += 23

	return offset, nil
}

// parseHandshakeResponseBody parse the HandshakeResponse (except the common header part).
func parseHandshakeResponseBody(ctx context.Context, packet *handshakeResponse41, data []byte, offset int) (err error) {
	defer func() {
		// Check malformat packet cause out of range is disgusting, but don't panic!
		if r := recover(); r != nil {
			logutil.Logger(ctx).Error("handshake panic", zap.ByteString("packetData", data))
			err = mysql.ErrMalformPacket
		}
	}()
	// user name
	packet.User = string(data[offset : offset+bytes.IndexByte(data[offset:], 0)])
	offset += len(packet.User) + 1

	if packet.Capability&mysql.ClientPluginAuthLenencClientData > 0 {
		// MySQL client sets the wrong capability, it will set this bit even server doesn't
		// support ClientPluginAuthLenencClientData.
		// https://github.com/mysql/mysql-server/blob/5.7/sql-common/client.c#L3478
		if data[offset] == 0x1 { // No auth data
			offset += 2
		} else {
			num, null, off := parseLengthEncodedInt(data[offset:])
			offset += off
			if !null {
				packet.Auth = data[offset : offset+int(num)]
				offset += int(num)
			}
		}
	} else if packet.Capability&mysql.ClientSecureConnection > 0 {
		// auth length and auth
		authLen := int(data[offset])
		offset++
		packet.Auth = data[offset : offset+authLen]
		offset += authLen
	} else {
		packet.Auth = data[offset : offset+bytes.IndexByte(data[offset:], 0)]
		offset += len(packet.Auth) + 1
	}

	if packet.Capability&mysql.ClientConnectWithDB > 0 {
		if len(data[offset:]) > 0 {
			idx := bytes.IndexByte(data[offset:], 0)
			packet.DBName = string(data[offset : offset+idx])
			offset += idx + 1
		}
	}

	if packet.Capability&mysql.ClientPluginAuth > 0 {
		idx := bytes.IndexByte(data[offset:], 0)
		s := offset
		f := offset + idx
		if s < f { // handle unexpected bad packets
			packet.AuthPlugin = string(data[s:f])
		}
		offset += idx + 1
	}

	if packet.Capability&mysql.ClientConnectAtts > 0 {
		if len(data[offset:]) == 0 {
			// Defend some ill-formated packet, connection attribute is not important and can be ignored.
			return nil
		}
		if num, null, off := parseLengthEncodedInt(data[offset:]); !null {
			offset += off
			row := data[offset : offset+int(num)]
			attrs, err := parseAttrs(row)
			if err != nil {
				logutil.Logger(ctx).Warn("parse attrs failed", zap.Error(err))
				return nil
			}
			packet.Attrs = attrs
		}
	}

	return nil
}

func parseAttrs(data []byte) (map[string]string, error) {
	attrs := make(map[string]string)
	pos := 0
	for pos < len(data) {
		key, _, off, err := parseLengthEncodedBytes(data[pos:])
		if err != nil {
			return attrs, err
		}
		pos += off
		value, _, off, err := parseLengthEncodedBytes(data[pos:])
		if err != nil {
			return attrs, err
		}
		pos += off

		attrs[string(key)] = string(value)
	}
	return attrs, nil
}

func (cc *clientConn) readOptionalSSLRequestAndHandshakeResponse(ctx context.Context) error {
	// Read a packet. It may be a SSLRequest or HandshakeResponse.
	data, err := cc.readPacket()
	if err != nil {
		err = errors.SuspendStack(err)
		if errors.Cause(err) == io.EOF {
			logutil.Logger(ctx).Debug("wait handshake response fail due to connection has be closed by client-side")
		} else {
			logutil.Logger(ctx).Debug("wait handshake response fail", zap.Error(err))
		}
		return err
	}

	isOldVersion := false

	var resp handshakeResponse41
	var pos int

	if len(data) < 2 {
		logutil.Logger(ctx).Error("got malformed handshake response", zap.ByteString("packetData", data))
		return mysql.ErrMalformPacket
	}

	capability := uint32(binary.LittleEndian.Uint16(data[:2]))
	if capability&mysql.ClientProtocol41 > 0 {
		pos, err = parseHandshakeResponseHeader(ctx, &resp, data)
	} else {
		pos, err = parseOldHandshakeResponseHeader(ctx, &resp, data)
		isOldVersion = true
	}

	if err != nil {
		terror.Log(err)
		return err
	}

	if resp.Capability&mysql.ClientSSL > 0 {
		tlsConfig := (*tls.Config)(atomic.LoadPointer(&cc.server.tlsConfig))
		if tlsConfig != nil {
			// The packet is a SSLRequest, let's switch to TLS.
			if err = cc.upgradeToTLS(tlsConfig); err != nil {
				return err
			}
			// Read the following HandshakeResponse packet.
			data, err = cc.readPacket()
			if err != nil {
				logutil.Logger(ctx).Warn("read handshake response failure after upgrade to TLS", zap.Error(err))
				return err
			}
			if isOldVersion {
				pos, err = parseOldHandshakeResponseHeader(ctx, &resp, data)
			} else {
				pos, err = parseHandshakeResponseHeader(ctx, &resp, data)
			}
			if err != nil {
				terror.Log(err)
				return err
			}
		}
	} else if config.GetGlobalConfig().Security.RequireSecureTransport {
		err := errSecureTransportRequired.FastGenByArgs()
		terror.Log(err)
		return err
	}

	// Read the remaining part of the packet.
	if isOldVersion {
		err = parseOldHandshakeResponseBody(ctx, &resp, data, pos)
	} else {
		err = parseHandshakeResponseBody(ctx, &resp, data, pos)
	}
	if err != nil {
		terror.Log(err)
		return err
	}

	cc.capability = resp.Capability & cc.server.capability
	cc.user = resp.User
	cc.dbname = resp.DBName
	cc.collation = resp.Collation
	cc.attrs = resp.Attrs

	err = cc.handleAuthPlugin(ctx, &resp)
	if err != nil {
		return err
	}

	switch resp.AuthPlugin {
	case mysql.AuthCachingSha2Password:
		resp.Auth, err = cc.authSha(ctx)
		if err != nil {
			return err
		}
	case mysql.AuthNativePassword:
	case mysql.AuthSocket:
	default:
		return errors.New("Unknown auth plugin")
	}

	err = cc.openSessionAndDoAuth(resp.Auth, resp.AuthPlugin)
	if err != nil {
		logutil.Logger(ctx).Warn("open new session or authentication failure", zap.Error(err))
	}
	return err
}

func (cc *clientConn) handleAuthPlugin(ctx context.Context, resp *handshakeResponse41) error {
	if resp.Capability&mysql.ClientPluginAuth > 0 {
		newAuth, err := cc.checkAuthPlugin(ctx, &resp.AuthPlugin)
		if err != nil {
			logutil.Logger(ctx).Warn("failed to check the user authplugin", zap.Error(err))
		}
		if len(newAuth) > 0 {
			resp.Auth = newAuth
		}

		switch resp.AuthPlugin {
		case mysql.AuthCachingSha2Password:
			resp.Auth, err = cc.authSha(ctx)
			if err != nil {
				return err
			}
		case mysql.AuthNativePassword:
		case mysql.AuthSocket:
		default:
			logutil.Logger(ctx).Warn("Unknown Auth Plugin", zap.String("plugin", resp.AuthPlugin))
		}
	} else {
		logutil.Logger(ctx).Warn("Client without Auth Plugin support; Please upgrade client")
	}
	return nil
}

func (cc *clientConn) authSha(ctx context.Context) ([]byte, error) {

	const (
		ShaCommand       = 1
		RequestRsaPubKey = 2
		FastAuthOk       = 3
		FastAuthFail     = 4
	)

	err := cc.writePacket([]byte{0, 0, 0, 0, ShaCommand, FastAuthFail})
	if err != nil {
		logutil.Logger(ctx).Error("authSha packet write failed", zap.Error(err))
		return nil, err
	}
	err = cc.flush(ctx)
	if err != nil {
		logutil.Logger(ctx).Error("authSha packet flush failed", zap.Error(err))
		return nil, err
	}

	data, err := cc.readPacket()
	if err != nil {
		logutil.Logger(ctx).Error("authSha packet read failed", zap.Error(err))
		return nil, err
	}
	return bytes.Trim(data, "\x00"), nil
}

func (cc *clientConn) SessionStatusToString() string {
	status := cc.ctx.Status()
	inTxn, autoCommit := 0, 0
	if status&mysql.ServerStatusInTrans > 0 {
		inTxn = 1
	}
	if status&mysql.ServerStatusAutocommit > 0 {
		autoCommit = 1
	}
	return fmt.Sprintf("inTxn:%d, autocommit:%d",
		inTxn, autoCommit,
	)
}

func (cc *clientConn) openSession() error {
	var tlsStatePtr *tls.ConnectionState
	if cc.tlsConn != nil {
		tlsState := cc.tlsConn.ConnectionState()
		tlsStatePtr = &tlsState
	}
	var err error
	cc.ctx, err = cc.server.driver.OpenCtx(cc.connectionID, cc.capability, cc.collation, cc.dbname, tlsStatePtr)
	if err != nil {
		return err
	}

	err = cc.server.checkConnectionCount()
	if err != nil {
		return err
	}
	return nil
}

func (cc *clientConn) openSessionAndDoAuth(authData []byte, authPlugin string) error {
	// Open a context unless this was done before.
	if cc.ctx == nil {
		err := cc.openSession()
		if err != nil {
			return err
		}
	}

	hasPassword := "YES"
	if len(authData) == 0 {
		hasPassword = "NO"
	}
	host, port, err := cc.PeerHost(hasPassword)
	if err != nil {
		return err
	}

	if !cc.isUnixSocket && authPlugin == mysql.AuthSocket {
		return errAccessDeniedNoPassword.FastGenByArgs(cc.user, host)
	}

	if !cc.ctx.Auth(&auth.UserIdentity{Username: cc.user, Hostname: host}, authData, cc.salt) {
		return errAccessDenied.FastGenByArgs(cc.user, host, hasPassword)
	}
	cc.ctx.SetPort(port)
	if cc.dbname != "" {
		err = cc.useDB(context.Background(), cc.dbname)
		if err != nil {
			return err
		}
	}
	cc.ctx.SetSessionManager(cc.server)
	return nil
}

// Check if the Authentication Plugin of the server, client and user configuration matches
func (cc *clientConn) checkAuthPlugin(ctx context.Context, authPlugin *string) ([]byte, error) {
	// Open a context unless this was done before.
	if cc.ctx == nil {
		err := cc.openSession()
		if err != nil {
			return nil, err
		}
	}

	userplugin, err := cc.ctx.AuthPluginForUser(&auth.UserIdentity{Username: cc.user, Hostname: cc.peerHost})
	if err != nil {
		return nil, err
	}
	if userplugin == mysql.AuthSocket {
		*authPlugin = mysql.AuthSocket
		user, err := user.LookupId(fmt.Sprint(cc.socketCredUID))
		if err != nil {
			return nil, err
		}
		return []byte(user.Username), nil
	}
	if len(userplugin) == 0 {
		logutil.Logger(ctx).Warn("No user plugin set, assuming MySQL Native Password",
			zap.String("user", cc.user), zap.String("host", cc.peerHost))
		*authPlugin = mysql.AuthNativePassword
		return nil, nil
	}

	// If the authentication method send by the server (cc.authPlugin) doesn't match
	// the plugin configured for the user account in the mysql.user.plugin column
	// or if the authentication method send by the server doesn't match the authentication
	// method send by the client (*authPlugin) then we need to switch the authentication
	// method to match the one configured for that specific user.
	if (cc.authPlugin != userplugin) || (cc.authPlugin != *authPlugin) {
		authData, err := cc.authSwitchRequest(ctx, userplugin)
		if err != nil {
			return nil, err
		}
		*authPlugin = userplugin
		return authData, nil
	}

	return nil, nil
}

func (cc *clientConn) PeerHost(hasPassword string) (host, port string, err error) {
	if len(cc.peerHost) > 0 {
		return cc.peerHost, "", nil
	}
	host = variable.DefHostname
	if cc.isUnixSocket {
		cc.peerHost = host
		return
	}
	addr := cc.bufReadConn.RemoteAddr().String()
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		err = errAccessDenied.GenWithStackByArgs(cc.user, addr, hasPassword)
		return
	}
	cc.peerHost = host
	cc.peerPort = port
	return
}

// skipInitConnect follows MySQL's rules of when init-connect should be skipped.
// In 5.7 it is any user with SUPER privilege, but in 8.0 it is:
// - SUPER or the CONNECTION_ADMIN dynamic privilege.
// - (additional exception) users with expired passwords (not yet supported)
// In TiDB CONNECTION_ADMIN is satisfied by SUPER, so we only need to check once.
func (cc *clientConn) skipInitConnect() bool {
	checker := privilege.GetPrivilegeManager(cc.ctx.Session)
	activeRoles := cc.ctx.GetSessionVars().ActiveRoles
	return checker != nil && checker.RequestDynamicVerification(activeRoles, "CONNECTION_ADMIN", false)
}

// initResultEncoder initialize the result encoder for current connection.
func (cc *clientConn) initResultEncoder(ctx context.Context) {
	chs, err := variable.GetSessionOrGlobalSystemVar(cc.ctx.GetSessionVars(), variable.CharacterSetResults)
	if err != nil {
		chs = ""
		logutil.Logger(ctx).Warn("get character_set_results system variable failed", zap.Error(err))
	}
	cc.rsEncoder = newResultEncoder(chs)
}

// initConnect runs the initConnect SQL statement if it has been specified.
// The semantics are MySQL compatible.
func (cc *clientConn) initConnect(ctx context.Context) error {
	val, err := cc.ctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(variable.InitConnect)
	if err != nil {
		return err
	}
	if val == "" || cc.skipInitConnect() {
		return nil
	}
	logutil.Logger(ctx).Debug("init_connect starting")
	stmts, err := cc.ctx.Parse(ctx, val)
	if err != nil {
		return err
	}
	for _, stmt := range stmts {
		rs, err := cc.ctx.ExecuteStmt(ctx, stmt)
		if err != nil {
			return err
		}
		// init_connect does not care about the results,
		// but they need to be drained because of lazy loading.
		if rs != nil {
			req := rs.NewChunk(nil)
			for {
				if err = rs.Next(ctx, req); err != nil {
					return err
				}
				if req.NumRows() == 0 {
					break
				}
			}
			if err := rs.Close(); err != nil {
				return err
			}
		}
	}
	logutil.Logger(ctx).Debug("init_connect complete")
	return nil
}

// Run reads client query and writes query result to client in for loop, if there is a panic during query handling,
// it will be recovered and log the panic error.
// This function returns and the connection is closed if there is an IO error or there is a panic.
func (cc *clientConn) Run(ctx context.Context) {
	const size = 4096
	defer func() {
		r := recover()
		if r != nil {
			buf := make([]byte, size)
			stackSize := runtime.Stack(buf, false)
			buf = buf[:stackSize]
			logutil.Logger(ctx).Error("connection running loop panic",
				zap.Stringer("lastSQL", getLastStmtInConn{cc}),
				zap.String("err", fmt.Sprintf("%v", r)),
				zap.String("stack", string(buf)),
			)
			err := cc.writeError(ctx, errors.New(fmt.Sprintf("%v", r)))
			terror.Log(err)
			metrics.PanicCounter.WithLabelValues(metrics.LabelSession).Inc()
		}
		if atomic.LoadInt32(&cc.status) != connStatusShutdown {
			err := cc.Close()
			terror.Log(err)
		}
	}()

	// Usually, client connection status changes between [dispatching] <=> [reading].
	// When some event happens, server may notify this client connection by setting
	// the status to special values, for example: kill or graceful shutdown.
	// The client connection would detect the events when it fails to change status
	// by CAS operation, it would then take some actions accordingly.
	for {
		if !atomic.CompareAndSwapInt32(&cc.status, connStatusDispatching, connStatusReading) ||
			// The judge below will not be hit by all means,
			// But keep it stayed as a reminder and for the code reference for connStatusWaitShutdown.
			atomic.LoadInt32(&cc.status) == connStatusWaitShutdown {
			return
		}

		cc.alloc.Reset()
		// close connection when idle time is more than wait_timeout
		waitTimeout := cc.getSessionVarsWaitTimeout(ctx)
		cc.pkt.setReadTimeout(time.Duration(waitTimeout) * time.Second)
		start := time.Now()
		data, err := cc.readPacket()
		if err != nil {
			if terror.ErrorNotEqual(err, io.EOF) {
				if netErr, isNetErr := errors.Cause(err).(net.Error); isNetErr && netErr.Timeout() {
					idleTime := time.Since(start)
					logutil.Logger(ctx).Info("read packet timeout, close this connection",
						zap.Duration("idle", idleTime),
						zap.Uint64("waitTimeout", waitTimeout),
						zap.Error(err),
					)
				} else {
					errStack := errors.ErrorStack(err)
					if !strings.Contains(errStack, "use of closed network connection") {
						logutil.Logger(ctx).Warn("read packet failed, close this connection",
							zap.Error(errors.SuspendStack(err)))
					}
				}
			}
			disconnectByClientWithError.Inc()
			return
		}

		if !atomic.CompareAndSwapInt32(&cc.status, connStatusReading, connStatusDispatching) {
			return
		}

		startTime := time.Now()
		err = cc.dispatch(ctx, data)
		cc.chunkAlloc.Reset()
		if err != nil {
			cc.audit(plugin.Error) // tell the plugin API there was a dispatch error
			if terror.ErrorEqual(err, io.EOF) {
				cc.addMetrics(data[0], startTime, nil)
				disconnectNormal.Inc()
				return
			} else if terror.ErrResultUndetermined.Equal(err) {
				logutil.Logger(ctx).Error("result undetermined, close this connection", zap.Error(err))
				disconnectErrorUndetermined.Inc()
				return
			} else if terror.ErrCritical.Equal(err) {
				metrics.CriticalErrorCounter.Add(1)
				logutil.Logger(ctx).Fatal("critical error, stop the server", zap.Error(err))
			}
			var txnMode string
			if cc.ctx != nil {
				txnMode = cc.ctx.GetSessionVars().GetReadableTxnMode()
			}
			metrics.ExecuteErrorCounter.WithLabelValues(metrics.ExecuteErrorToLabel(err)).Inc()
			if storeerr.ErrLockAcquireFailAndNoWaitSet.Equal(err) {
				logutil.Logger(ctx).Debug("Expected error for FOR UPDATE NOWAIT", zap.Error(err))
			} else {
				logutil.Logger(ctx).Info("command dispatched failed",
					zap.String("connInfo", cc.String()),
					zap.String("command", mysql.Command2Str[data[0]]),
					zap.String("status", cc.SessionStatusToString()),
					zap.Stringer("sql", getLastStmtInConn{cc}),
					zap.String("txn_mode", txnMode),
					zap.String("err", errStrForLog(err, cc.ctx.GetSessionVars().EnableRedactLog)),
				)
			}
			err1 := cc.writeError(ctx, err)
			terror.Log(err1)
		}
		cc.addMetrics(data[0], startTime, err)
		cc.pkt.sequence = 0
	}
}

// ShutdownOrNotify will Shutdown this client connection, or do its best to notify.
func (cc *clientConn) ShutdownOrNotify() bool {
	if (cc.ctx.Status() & mysql.ServerStatusInTrans) > 0 {
		return false
	}
	// If the client connection status is reading, it's safe to shutdown it.
	if atomic.CompareAndSwapInt32(&cc.status, connStatusReading, connStatusShutdown) {
		return true
	}
	// If the client connection status is dispatching, we can't shutdown it immediately,
	// so set the status to WaitShutdown as a notification, the loop in clientConn.Run
	// will detect it and then exit.
	atomic.StoreInt32(&cc.status, connStatusWaitShutdown)
	return false
}

func errStrForLog(err error, enableRedactLog bool) string {
	if enableRedactLog {
		// currently, only ErrParse is considered when enableRedactLog because it may contain sensitive information like
		// password or accesskey
		if parser.ErrParse.Equal(err) {
			return "fail to parse SQL and can't redact when enable log redaction"
		}
	}
	if kv.ErrKeyExists.Equal(err) || parser.ErrParse.Equal(err) || infoschema.ErrTableNotExists.Equal(err) {
		// Do not log stack for duplicated entry error.
		return err.Error()
	}
	return errors.ErrorStack(err)
}

func (cc *clientConn) addMetrics(cmd byte, startTime time.Time, err error) {
	if cmd == mysql.ComQuery && cc.ctx.Value(sessionctx.LastExecuteDDL) != nil {
		// Don't take DDL execute time into account.
		// It's already recorded by other metrics in ddl package.
		return
	}

	var counter prometheus.Counter
	if err != nil && int(cmd) < len(queryTotalCountErr) {
		counter = queryTotalCountErr[cmd]
	} else if err == nil && int(cmd) < len(queryTotalCountOk) {
		counter = queryTotalCountOk[cmd]
	}
	if counter != nil {
		counter.Inc()
	} else {
		label := strconv.Itoa(int(cmd))
		if err != nil {
			metrics.QueryTotalCounter.WithLabelValues(label, "Error").Inc()
		} else {
			metrics.QueryTotalCounter.WithLabelValues(label, "OK").Inc()
		}
	}

	stmtType := cc.ctx.GetSessionVars().StmtCtx.StmtType
	sqlType := metrics.LblGeneral
	if stmtType != "" {
		sqlType = stmtType
	}

	cost := time.Since(startTime)
	sessionVar := cc.ctx.GetSessionVars()
	cc.ctx.GetTxnWriteThroughputSLI().FinishExecuteStmt(cost, cc.ctx.AffectedRows(), sessionVar.InTxn())

	switch sqlType {
	case "Use":
		queryDurationHistogramUse.Observe(cost.Seconds())
	case "Show":
		queryDurationHistogramShow.Observe(cost.Seconds())
	case "Begin":
		queryDurationHistogramBegin.Observe(cost.Seconds())
	case "Commit":
		queryDurationHistogramCommit.Observe(cost.Seconds())
	case "Rollback":
		queryDurationHistogramRollback.Observe(cost.Seconds())
	case "Insert":
		queryDurationHistogramInsert.Observe(cost.Seconds())
	case "Replace":
		queryDurationHistogramReplace.Observe(cost.Seconds())
	case "Delete":
		queryDurationHistogramDelete.Observe(cost.Seconds())
	case "Update":
		queryDurationHistogramUpdate.Observe(cost.Seconds())
	case "Select":
		queryDurationHistogramSelect.Observe(cost.Seconds())
	case "Execute":
		queryDurationHistogramExecute.Observe(cost.Seconds())
	case "Set":
		queryDurationHistogramSet.Observe(cost.Seconds())
	case metrics.LblGeneral:
		queryDurationHistogramGeneral.Observe(cost.Seconds())
	default:
		metrics.QueryDurationHistogram.WithLabelValues(sqlType).Observe(cost.Seconds())
	}
}

// dispatch handles client request based on command which is the first byte of the data.
// It also gets a token from server which is used to limit the concurrently handling clients.
// The most frequently used command is ComQuery.
func (cc *clientConn) dispatch(ctx context.Context, data []byte) error {
	defer func() {
		// reset killed for each request
		atomic.StoreUint32(&cc.ctx.GetSessionVars().Killed, 0)
	}()
	t := time.Now()
	if (cc.ctx.Status() & mysql.ServerStatusInTrans) > 0 {
		connIdleDurationHistogramInTxn.Observe(t.Sub(cc.lastActive).Seconds())
	} else {
		connIdleDurationHistogramNotInTxn.Observe(t.Sub(cc.lastActive).Seconds())
	}

	span := opentracing.StartSpan("server.dispatch")
	cfg := config.GetGlobalConfig()
	if cfg.OpenTracing.Enable {
		ctx = opentracing.ContextWithSpan(ctx, span)
	}

	var cancelFunc context.CancelFunc
	ctx, cancelFunc = context.WithCancel(ctx)
	cc.mu.Lock()
	cc.mu.cancelFunc = cancelFunc
	cc.mu.Unlock()

	cc.lastPacket = data
	cmd := data[0]
	data = data[1:]
	if variable.TopSQLEnabled() {
		defer pprof.SetGoroutineLabels(ctx)
	}
	if variable.EnablePProfSQLCPU.Load() {
		label := getLastStmtInConn{cc}.PProfLabel()
		if len(label) > 0 {
			defer pprof.SetGoroutineLabels(ctx)
			ctx = pprof.WithLabels(ctx, pprof.Labels("sql", label))
			pprof.SetGoroutineLabels(ctx)
		}
	}
	if trace.IsEnabled() {
		lc := getLastStmtInConn{cc}
		sqlType := lc.PProfLabel()
		if len(sqlType) > 0 {
			var task *trace.Task
			ctx, task = trace.NewTask(ctx, sqlType)
			defer task.End()

			trace.Log(ctx, "sql", lc.String())
			ctx = logutil.WithTraceLogger(ctx, cc.connectionID)

			taskID := *(*uint64)(unsafe.Pointer(task))
			ctx = pprof.WithLabels(ctx, pprof.Labels("trace", strconv.FormatUint(taskID, 10)))
			pprof.SetGoroutineLabels(ctx)
		}
	}
	token := cc.server.getToken()
	defer func() {
		// if handleChangeUser failed, cc.ctx may be nil
		if cc.ctx != nil {
			cc.ctx.SetProcessInfo("", t, mysql.ComSleep, 0)
		}

		cc.server.releaseToken(token)
		span.Finish()
		cc.lastActive = time.Now()
	}()

	vars := cc.ctx.GetSessionVars()
	// reset killed for each request
	atomic.StoreUint32(&vars.Killed, 0)
	if cmd < mysql.ComEnd {
		cc.ctx.SetCommandValue(cmd)
	}

	dataStr := string(hack.String(data))
	switch cmd {
	case mysql.ComPing, mysql.ComStmtClose, mysql.ComStmtSendLongData, mysql.ComStmtReset,
		mysql.ComSetOption, mysql.ComChangeUser:
		cc.ctx.SetProcessInfo("", t, cmd, 0)
	case mysql.ComInitDB:
		cc.ctx.SetProcessInfo("use "+dataStr, t, cmd, 0)
	}

	switch cmd {
	case mysql.ComSleep:
		// TODO: According to mysql document, this command is supposed to be used only internally.
		// So it's just a temp fix, not sure if it's done right.
		// Investigate this command and write test case later.
		return nil
	case mysql.ComQuit:
		return io.EOF
	case mysql.ComInitDB:
		if err := cc.useDB(ctx, dataStr); err != nil {
			return err
		}
		return cc.writeOK(ctx)
	case mysql.ComQuery: // Most frequently used command.
		// For issue 1989
		// Input payload may end with byte '\0', we didn't find related mysql document about it, but mysql
		// implementation accept that case. So trim the last '\0' here as if the payload an EOF string.
		// See http://dev.mysql.com/doc/internals/en/com-query.html
		if len(data) > 0 && data[len(data)-1] == 0 {
			data = data[:len(data)-1]
			dataStr = string(hack.String(data))
		}
		return cc.handleQuery(ctx, dataStr)
	case mysql.ComFieldList:
		return cc.handleFieldList(ctx, dataStr)
	// ComCreateDB, ComDropDB
	case mysql.ComRefresh:
		return cc.handleRefresh(ctx, data[0])
	case mysql.ComShutdown: // redirect to SQL
		if err := cc.handleQuery(ctx, "SHUTDOWN"); err != nil {
			return err
		}
		return cc.writeOK(ctx)
	case mysql.ComStatistics:
		return cc.writeStats(ctx)
	// ComProcessInfo, ComConnect, ComProcessKill, ComDebug
	case mysql.ComPing:
		return cc.writeOK(ctx)
	case mysql.ComChangeUser:
		return cc.handleChangeUser(ctx, data)
	// ComBinlogDump, ComTableDump, ComConnectOut, ComRegisterSlave
	case mysql.ComStmtPrepare:
		return cc.handleStmtPrepare(ctx, dataStr)
	case mysql.ComStmtExecute:
		return cc.handleStmtExecute(ctx, data)
	case mysql.ComStmtSendLongData:
		return cc.handleStmtSendLongData(data)
	case mysql.ComStmtClose:
		return cc.handleStmtClose(data)
	case mysql.ComStmtReset:
		return cc.handleStmtReset(ctx, data)
	case mysql.ComSetOption:
		return cc.handleSetOption(ctx, data)
	case mysql.ComStmtFetch:
		return cc.handleStmtFetch(ctx, data)
	// ComDaemon, ComBinlogDumpGtid
	case mysql.ComResetConnection:
		return cc.handleResetConnection(ctx)
	// ComEnd
	default:
		return mysql.NewErrf(mysql.ErrUnknown, "command %d not supported now", nil, cmd)
	}
}

func (cc *clientConn) writeStats(ctx context.Context) error {
	msg := []byte("Uptime: 0  Threads: 0  Questions: 0  Slow queries: 0  Opens: 0  Flush tables: 0  Open tables: 0  Queries per second avg: 0.000")
	data := cc.alloc.AllocWithLen(4, len(msg))
	data = append(data, msg...)

	err := cc.writePacket(data)
	if err != nil {
		return err
	}

	return cc.flush(ctx)
}

func (cc *clientConn) useDB(ctx context.Context, db string) (err error) {
	// if input is "use `SELECT`", mysql client just send "SELECT"
	// so we add `` around db.
	stmts, err := cc.ctx.Parse(ctx, "use `"+db+"`")
	if err != nil {
		return err
	}
	_, err = cc.ctx.ExecuteStmt(ctx, stmts[0])
	if err != nil {
		return err
	}
	cc.dbname = db
	return
}

func (cc *clientConn) flush(ctx context.Context) error {
	defer func() {
		trace.StartRegion(ctx, "FlushClientConn").End()
		if cc.ctx != nil && cc.ctx.WarningCount() > 0 {
			for _, err := range cc.ctx.GetWarnings() {
				var warn *errors.Error
				if ok := goerr.As(err.Err, &warn); ok {
					code := uint16(warn.Code())
					errno.IncrementWarning(code, cc.user, cc.peerHost)
				}
			}
		}
	}()
	failpoint.Inject("FakeClientConn", func() {
		if cc.pkt == nil {
			failpoint.Return(nil)
		}
	})
	return cc.pkt.flush()
}

func (cc *clientConn) writeOK(ctx context.Context) error {
	msg := cc.ctx.LastMessage()
	return cc.writeOkWith(ctx, msg, cc.ctx.AffectedRows(), cc.ctx.LastInsertID(), cc.ctx.Status(), cc.ctx.WarningCount())
}

func (cc *clientConn) writeOkWith(ctx context.Context, msg string, affectedRows, lastInsertID uint64, status, warnCnt uint16) error {
	enclen := 0
	if len(msg) > 0 {
		enclen = lengthEncodedIntSize(uint64(len(msg))) + len(msg)
	}

	data := cc.alloc.AllocWithLen(4, 32+enclen)
	data = append(data, mysql.OKHeader)
	data = dumpLengthEncodedInt(data, affectedRows)
	data = dumpLengthEncodedInt(data, lastInsertID)
	if cc.capability&mysql.ClientProtocol41 > 0 {
		data = dumpUint16(data, status)
		data = dumpUint16(data, warnCnt)
	}
	if enclen > 0 {
		// although MySQL manual says the info message is string<EOF>(https://dev.mysql.com/doc/internals/en/packet-OK_Packet.html),
		// it is actually string<lenenc>
		data = dumpLengthEncodedString(data, []byte(msg))
	}

	err := cc.writePacket(data)
	if err != nil {
		return err
	}

	return cc.flush(ctx)
}

func (cc *clientConn) writeError(ctx context.Context, e error) error {
	var (
		m  *mysql.SQLError
		te *terror.Error
		ok bool
	)
	originErr := errors.Cause(e)
	if te, ok = originErr.(*terror.Error); ok {
		m = terror.ToSQLError(te)
	} else {
		e := errors.Cause(originErr)
		switch y := e.(type) {
		case *terror.Error:
			m = terror.ToSQLError(y)
		default:
			m = mysql.NewErrf(mysql.ErrUnknown, "%s", nil, e.Error())
		}
	}

	cc.lastCode = m.Code
	defer errno.IncrementError(m.Code, cc.user, cc.peerHost)
	data := cc.alloc.AllocWithLen(4, 16+len(m.Message))
	data = append(data, mysql.ErrHeader)
	data = append(data, byte(m.Code), byte(m.Code>>8))
	if cc.capability&mysql.ClientProtocol41 > 0 {
		data = append(data, '#')
		data = append(data, m.State...)
	}

	data = append(data, m.Message...)

	err := cc.writePacket(data)
	if err != nil {
		return err
	}
	return cc.flush(ctx)
}

// writeEOF writes an EOF packet.
// Note this function won't flush the stream because maybe there are more
// packets following it.
// serverStatus, a flag bit represents server information
// in the packet.
func (cc *clientConn) writeEOF(serverStatus uint16) error {
	data := cc.alloc.AllocWithLen(4, 9)

	data = append(data, mysql.EOFHeader)
	if cc.capability&mysql.ClientProtocol41 > 0 {
		data = dumpUint16(data, cc.ctx.WarningCount())
		status := cc.ctx.Status()
		status |= serverStatus
		data = dumpUint16(data, status)
	}

	err := cc.writePacket(data)
	return err
}

func (cc *clientConn) writeReq(ctx context.Context, filePath string) error {
	data := cc.alloc.AllocWithLen(4, 5+len(filePath))
	data = append(data, mysql.LocalInFileHeader)
	data = append(data, filePath...)

	err := cc.writePacket(data)
	if err != nil {
		return err
	}

	return cc.flush(ctx)
}

func insertDataWithCommit(ctx context.Context, prevData,
	curData []byte, loadDataInfo *executor.LoadDataInfo) ([]byte, error) {
	var err error
	var reachLimit bool
	for {
		prevData, reachLimit, err = loadDataInfo.InsertData(ctx, prevData, curData)
		if err != nil {
			return nil, err
		}
		if !reachLimit {
			break
		}
		// push into commit task queue
		err = loadDataInfo.EnqOneTask(ctx)
		if err != nil {
			return prevData, err
		}
		curData = prevData
		prevData = nil
	}
	return prevData, nil
}

// processStream process input stream from network
func processStream(ctx context.Context, cc *clientConn, loadDataInfo *executor.LoadDataInfo, wg *sync.WaitGroup) {
	var err error
	var shouldBreak bool
	var prevData, curData []byte
	defer func() {
		r := recover()
		if r != nil {
			logutil.Logger(ctx).Error("process routine panicked",
				zap.Reflect("r", r),
				zap.Stack("stack"))
		}
		if err != nil || r != nil {
			loadDataInfo.ForceQuit()
		} else {
			loadDataInfo.CloseTaskQueue()
		}
		wg.Done()
	}()
	for {
		curData, err = cc.readPacket()
		if err != nil {
			if terror.ErrorNotEqual(err, io.EOF) {
				logutil.Logger(ctx).Error("read packet failed", zap.Error(err))
				break
			}
		}
		if len(curData) == 0 {
			loadDataInfo.Drained = true
			shouldBreak = true
			if len(prevData) == 0 {
				break
			}
		}
		select {
		case <-loadDataInfo.QuitCh:
			err = errors.New("processStream forced to quit")
		default:
		}
		if err != nil {
			break
		}
		// prepare batch and enqueue task
		prevData, err = insertDataWithCommit(ctx, prevData, curData, loadDataInfo)
		if err != nil {
			break
		}
		if shouldBreak {
			break
		}
	}
	if err != nil {
		logutil.Logger(ctx).Error("load data process stream error", zap.Error(err))
		return
	}
	if err = loadDataInfo.EnqOneTask(ctx); err != nil {
		logutil.Logger(ctx).Error("load data process stream error", zap.Error(err))
		return
	}
}

// handleLoadData does the additional work after processing the 'load data' query.
// It sends client a file path, then reads the file content from client, inserts data into database.
func (cc *clientConn) handleLoadData(ctx context.Context, loadDataInfo *executor.LoadDataInfo) error {
	// If the server handles the load data request, the client has to set the ClientLocalFiles capability.
	if cc.capability&mysql.ClientLocalFiles == 0 {
		return errNotAllowedCommand
	}
	if loadDataInfo == nil {
		return errors.New("load data info is empty")
	}
	if !loadDataInfo.Table.Meta().IsBaseTable() {
		return errors.New("can only load data into base tables")
	}
	err := cc.writeReq(ctx, loadDataInfo.Path)
	if err != nil {
		return err
	}

	loadDataInfo.InitQueues()
	loadDataInfo.SetMaxRowsInBatch(uint64(loadDataInfo.Ctx.GetSessionVars().DMLBatchSize))
	loadDataInfo.StartStopWatcher()
	// let stop watcher goroutine quit
	defer loadDataInfo.ForceQuit()
	err = loadDataInfo.Ctx.NewTxn(ctx)
	if err != nil {
		return err
	}
	// processStream process input data, enqueue commit task
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go processStream(ctx, cc, loadDataInfo, wg)
	err = loadDataInfo.CommitWork(ctx)
	wg.Wait()
	if err != nil {
		if !loadDataInfo.Drained {
			logutil.Logger(ctx).Info("not drained yet, try reading left data from client connection")
		}
		// drain the data from client conn util empty packet received, otherwise the connection will be reset
		for !loadDataInfo.Drained {
			// check kill flag again, let the draining loop could quit if empty packet could not be received
			if atomic.CompareAndSwapUint32(&loadDataInfo.Ctx.GetSessionVars().Killed, 1, 0) {
				logutil.Logger(ctx).Warn("receiving kill, stop draining data, connection may be reset")
				return executor.ErrQueryInterrupted
			}
			curData, err1 := cc.readPacket()
			if err1 != nil {
				logutil.Logger(ctx).Error("drain reading left data encounter errors", zap.Error(err1))
				break
			}
			if len(curData) == 0 {
				loadDataInfo.Drained = true
				logutil.Logger(ctx).Info("draining finished for error", zap.Error(err))
				break
			}
		}
	}
	loadDataInfo.SetMessage()
	return err
}

// getDataFromPath gets file contents from file path.
func (cc *clientConn) getDataFromPath(ctx context.Context, path string) ([]byte, error) {
	err := cc.writeReq(ctx, path)
	if err != nil {
		return nil, err
	}
	var prevData, curData []byte
	for {
		curData, err = cc.readPacket()
		if err != nil && terror.ErrorNotEqual(err, io.EOF) {
			return nil, err
		}
		if len(curData) == 0 {
			break
		}
		prevData = append(prevData, curData...)
	}
	return prevData, nil
}

// handleLoadStats does the additional work after processing the 'load stats' query.
// It sends client a file path, then reads the file content from client, loads it into the storage.
func (cc *clientConn) handleLoadStats(ctx context.Context, loadStatsInfo *executor.LoadStatsInfo) error {
	// If the server handles the load data request, the client has to set the ClientLocalFiles capability.
	if cc.capability&mysql.ClientLocalFiles == 0 {
		return errNotAllowedCommand
	}
	if loadStatsInfo == nil {
		return errors.New("load stats: info is empty")
	}
	data, err := cc.getDataFromPath(ctx, loadStatsInfo.Path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return loadStatsInfo.Update(data)
}

// handleIndexAdvise does the index advise work and returns the advise result for index.
func (cc *clientConn) handleIndexAdvise(ctx context.Context, indexAdviseInfo *executor.IndexAdviseInfo) error {
	if cc.capability&mysql.ClientLocalFiles == 0 {
		return errNotAllowedCommand
	}
	if indexAdviseInfo == nil {
		return errors.New("Index Advise: info is empty")
	}

	data, err := cc.getDataFromPath(ctx, indexAdviseInfo.Path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("Index Advise: infile is empty")
	}

	if err := indexAdviseInfo.GetIndexAdvice(ctx, data); err != nil {
		return err
	}

	// TODO: Write the rss []ResultSet. It will be done in another PR.
	return nil
}

func (cc *clientConn) handlePlanReplayerLoad(ctx context.Context, planReplayerLoadInfo *executor.PlanReplayerLoadInfo) error {
	if cc.capability&mysql.ClientLocalFiles == 0 {
		return errNotAllowedCommand
	}
	if planReplayerLoadInfo == nil {
		return errors.New("plan replayer load: info is empty")
	}
	data, err := cc.getDataFromPath(ctx, planReplayerLoadInfo.Path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return planReplayerLoadInfo.Update(data)
}

func (cc *clientConn) audit(eventType plugin.GeneralEvent) {
	err := plugin.ForeachPlugin(plugin.Audit, func(p *plugin.Plugin) error {
		audit := plugin.DeclareAuditManifest(p.Manifest)
		if audit.OnGeneralEvent != nil {
			cmd := mysql.Command2Str[byte(atomic.LoadUint32(&cc.ctx.GetSessionVars().CommandValue))]
			ctx := context.WithValue(context.Background(), plugin.ExecStartTimeCtxKey, cc.ctx.GetSessionVars().StartTime)
			audit.OnGeneralEvent(ctx, cc.ctx.GetSessionVars(), eventType, cmd)
		}
		return nil
	})
	if err != nil {
		terror.Log(err)
	}
}

// handleQuery executes the sql query string and writes result set or result ok to the client.
// As the execution time of this function represents the performance of TiDB, we do time log and metrics here.
// There is a special query `load data` that does not return result, which is handled differently.
// Query `load stats` does not return result either.
func (cc *clientConn) handleQuery(ctx context.Context, sql string) (err error) {
	defer trace.StartRegion(ctx, "handleQuery").End()
	sc := cc.ctx.GetSessionVars().StmtCtx
	prevWarns := sc.GetWarnings()
	stmts, err := cc.ctx.Parse(ctx, sql)
	if err != nil {
		return err
	}

	if len(stmts) == 0 {
		return cc.writeOK(ctx)
	}

	warns := sc.GetWarnings()
	parserWarns := warns[len(prevWarns):]

	var pointPlans []plannercore.Plan
	if len(stmts) > 1 {

		// The client gets to choose if it allows multi-statements, and
		// probably defaults OFF. This helps prevent against SQL injection attacks
		// by early terminating the first statement, and then running an entirely
		// new statement.

		capabilities := cc.ctx.GetSessionVars().ClientCapability
		if capabilities&mysql.ClientMultiStatements < 1 {
			// The client does not have multi-statement enabled. We now need to determine
			// how to handle an unsafe situation based on the multiStmt sysvar.
			switch cc.ctx.GetSessionVars().MultiStatementMode {
			case variable.OffInt:
				err = errMultiStatementDisabled
				return err
			case variable.OnInt:
				// multi statement is fully permitted, do nothing
			default:
				warn := stmtctx.SQLWarn{Level: stmtctx.WarnLevelWarning, Err: errMultiStatementDisabled}
				parserWarns = append(parserWarns, warn)
			}
		}

		// Only pre-build point plans for multi-statement query
		pointPlans, err = cc.prefetchPointPlanKeys(ctx, stmts)
		if err != nil {
			return err
		}
	}
	if len(pointPlans) > 0 {
		defer cc.ctx.ClearValue(plannercore.PointPlanKey)
	}
	var retryable bool
	for i, stmt := range stmts {
		if len(pointPlans) > 0 {
			// Save the point plan in Session, so we don't need to build the point plan again.
			cc.ctx.SetValue(plannercore.PointPlanKey, plannercore.PointPlanVal{Plan: pointPlans[i]})
		}
		retryable, err = cc.handleStmt(ctx, stmt, parserWarns, i == len(stmts)-1)
		if err != nil {
			if !retryable || !errors.ErrorEqual(err, storeerr.ErrTiFlashServerTimeout) {
				break
			}
			_, allowTiFlashFallback := cc.ctx.GetSessionVars().AllowFallbackToTiKV[kv.TiFlash]
			if !allowTiFlashFallback {
				break
			}
			// When the TiFlash server seems down, we append a warning to remind the user to check the status of the TiFlash
			// server and fallback to TiKV.
			warns := append(parserWarns, stmtctx.SQLWarn{Level: stmtctx.WarnLevelError, Err: err})
			delete(cc.ctx.GetSessionVars().IsolationReadEngines, kv.TiFlash)
			_, err = cc.handleStmt(ctx, stmt, warns, i == len(stmts)-1)
			cc.ctx.GetSessionVars().IsolationReadEngines[kv.TiFlash] = struct{}{}
			if err != nil {
				break
			}
		}
	}
	return err
}

// prefetchPointPlanKeys extracts the point keys in multi-statement query,
// use BatchGet to get the keys, so the values will be cached in the snapshot cache, save RPC call cost.
// For pessimistic transaction, the keys will be batch locked.
func (cc *clientConn) prefetchPointPlanKeys(ctx context.Context, stmts []ast.StmtNode) ([]plannercore.Plan, error) {
	txn, err := cc.ctx.Txn(false)
	if err != nil {
		return nil, err
	}
	if !txn.Valid() {
		// Only prefetch in-transaction query for simplicity.
		// Later we can support out-transaction multi-statement query.
		return nil, nil
	}
	vars := cc.ctx.GetSessionVars()
	if vars.TxnCtx.IsPessimistic {
		if vars.IsIsolation(ast.ReadCommitted) {
			// TODO: to support READ-COMMITTED, we need to avoid getting new TS for each statement in the query.
			return nil, nil
		}
		if vars.TxnCtx.GetForUpdateTS() != vars.TxnCtx.StartTS {
			// Do not handle the case that ForUpdateTS is changed for simplicity.
			return nil, nil
		}
	}
	pointPlans := make([]plannercore.Plan, len(stmts))
	var idxKeys []kv.Key
	var rowKeys []kv.Key
	sc := vars.StmtCtx
	for i, stmt := range stmts {
		switch stmt.(type) {
		case *ast.UseStmt:
			// If there is a "use db" statement, we shouldn't cache even if it's possible.
			// Consider the scenario where there are statements that could execute on multiple
			// schemas, but the schema is actually different.
			return nil, nil
		}
		// TODO: the preprocess is run twice, we should find some way to avoid do it again.
		// TODO: handle the PreprocessorReturn.
		if err = plannercore.Preprocess(cc.ctx, stmt); err != nil {
			return nil, err
		}
		p := plannercore.TryFastPlan(cc.ctx.Session, stmt)
		pointPlans[i] = p
		if p == nil {
			continue
		}
		// Only support Update for now.
		// TODO: support other point plans.
		switch x := p.(type) {
		case *plannercore.Update:
			updateStmt := stmt.(*ast.UpdateStmt)
			if pp, ok := x.SelectPlan.(*plannercore.PointGetPlan); ok {
				if pp.PartitionInfo != nil {
					continue
				}
				if pp.IndexInfo != nil {
					executor.ResetUpdateStmtCtx(sc, updateStmt, vars)
					idxKey, err1 := executor.EncodeUniqueIndexKey(cc.ctx, pp.TblInfo, pp.IndexInfo, pp.IndexValues, pp.TblInfo.ID)
					if err1 != nil {
						return nil, err1
					}
					idxKeys = append(idxKeys, idxKey)
				} else {
					rowKeys = append(rowKeys, tablecodec.EncodeRowKeyWithHandle(pp.TblInfo.ID, pp.Handle))
				}
			}
		}
	}
	if len(idxKeys) == 0 && len(rowKeys) == 0 {
		return pointPlans, nil
	}
	snapshot := txn.GetSnapshot()
	idxVals, err1 := snapshot.BatchGet(ctx, idxKeys)
	if err1 != nil {
		return nil, err1
	}
	for idxKey, idxVal := range idxVals {
		h, err2 := tablecodec.DecodeHandleInUniqueIndexValue(idxVal, false)
		if err2 != nil {
			return nil, err2
		}
		tblID := tablecodec.DecodeTableID(hack.Slice(idxKey))
		rowKeys = append(rowKeys, tablecodec.EncodeRowKeyWithHandle(tblID, h))
	}
	if vars.TxnCtx.IsPessimistic {
		allKeys := append(rowKeys, idxKeys...)
		err = executor.LockKeys(ctx, cc.ctx, vars.LockWaitTimeout, allKeys...)
		if err != nil {
			// suppress the lock error, we are not going to handle it here for simplicity.
			err = nil
			logutil.BgLogger().Warn("lock keys error on prefetch", zap.Error(err))
		}
	} else {
		_, err = snapshot.BatchGet(ctx, rowKeys)
		if err != nil {
			return nil, err
		}
	}
	return pointPlans, nil
}

// The first return value indicates whether the call of handleStmt has no side effect and can be retried.
// Currently, the first return value is used to fall back to TiKV when TiFlash is down.
func (cc *clientConn) handleStmt(ctx context.Context, stmt ast.StmtNode, warns []stmtctx.SQLWarn, lastStmt bool) (bool, error) {
	ctx = context.WithValue(ctx, execdetails.StmtExecDetailKey, &execdetails.StmtExecDetails{})
	ctx = context.WithValue(ctx, util.ExecDetailsKey, &util.ExecDetails{})
	reg := trace.StartRegion(ctx, "ExecuteStmt")
	cc.audit(plugin.Starting)
	rs, err := cc.ctx.ExecuteStmt(ctx, stmt)
	reg.End()
	// The session tracker detachment from global tracker is solved in the `rs.Close` in most cases.
	// If the rs is nil, the detachment will be done in the `handleNoDelay`.
	if rs != nil {
		defer terror.Call(rs.Close)
	}
	if err != nil {
		return true, err
	}

	status := cc.ctx.Status()
	if lastStmt {
		cc.ctx.GetSessionVars().StmtCtx.AppendWarnings(warns)
	} else {
		status |= mysql.ServerMoreResultsExists
	}

	if rs != nil {
		if connStatus := atomic.LoadInt32(&cc.status); connStatus == connStatusShutdown {
			return false, executor.ErrQueryInterrupted
		}
		if retryable, err := cc.writeResultset(ctx, rs, false, status, 0); err != nil {
			return retryable, err
		}
		return false, nil
	}

	handled, err := cc.handleQuerySpecial(ctx, status)
	if handled {
		if execStmt := cc.ctx.Value(session.ExecStmtVarKey); execStmt != nil {
			execStmt.(*executor.ExecStmt).FinishExecuteStmt(0, err, false)
		}
	}
	if err != nil {
		return false, err
	}

	return false, nil
}

func (cc *clientConn) handleQuerySpecial(ctx context.Context, status uint16) (bool, error) {
	handled := false
	loadDataInfo := cc.ctx.Value(executor.LoadDataVarKey)
	if loadDataInfo != nil {
		handled = true
		defer cc.ctx.SetValue(executor.LoadDataVarKey, nil)
		if err := cc.handleLoadData(ctx, loadDataInfo.(*executor.LoadDataInfo)); err != nil {
			return handled, err
		}
	}

	loadStats := cc.ctx.Value(executor.LoadStatsVarKey)
	if loadStats != nil {
		handled = true
		defer cc.ctx.SetValue(executor.LoadStatsVarKey, nil)
		if err := cc.handleLoadStats(ctx, loadStats.(*executor.LoadStatsInfo)); err != nil {
			return handled, err
		}
	}

	indexAdvise := cc.ctx.Value(executor.IndexAdviseVarKey)
	if indexAdvise != nil {
		handled = true
		defer cc.ctx.SetValue(executor.IndexAdviseVarKey, nil)
		if err := cc.handleIndexAdvise(ctx, indexAdvise.(*executor.IndexAdviseInfo)); err != nil {
			return handled, err
		}
	}

	planReplayerLoad := cc.ctx.Value(executor.PlanReplayerLoadVarKey)
	if planReplayerLoad != nil {
		handled = true
		defer cc.ctx.SetValue(executor.PlanReplayerLoadVarKey, nil)
		if err := cc.handlePlanReplayerLoad(ctx, planReplayerLoad.(*executor.PlanReplayerLoadInfo)); err != nil {
			return handled, err
		}
	}

	return handled, cc.writeOkWith(ctx, cc.ctx.LastMessage(), cc.ctx.AffectedRows(), cc.ctx.LastInsertID(), status, cc.ctx.WarningCount())
}

// handleFieldList returns the field list for a table.
// The sql string is composed of a table name and a terminating character \x00.
func (cc *clientConn) handleFieldList(ctx context.Context, sql string) (err error) {
	parts := strings.Split(sql, "\x00")
	columns, err := cc.ctx.FieldList(parts[0])
	if err != nil {
		return err
	}
	data := cc.alloc.AllocWithLen(4, 1024)
	cc.initResultEncoder(ctx)
	defer cc.rsEncoder.clean()
	for _, column := range columns {
		// Current we doesn't output defaultValue but reserve defaultValue length byte to make mariadb client happy.
		// https://dev.mysql.com/doc/internals/en/com-query-response.html#column-definition
		// TODO: fill the right DefaultValues.
		column.DefaultValueLength = 0
		column.DefaultValue = []byte{}

		data = data[0:4]
		data = column.Dump(data, cc.rsEncoder)
		if err := cc.writePacket(data); err != nil {
			return err
		}
	}
	if err := cc.writeEOF(0); err != nil {
		return err
	}
	return cc.flush(ctx)
}

// writeResultset writes data into a resultset and uses rs.Next to get row data back.
// If binary is true, the data would be encoded in BINARY format.
// serverStatus, a flag bit represents server information.
// fetchSize, the desired number of rows to be fetched each time when client uses cursor.
// retryable indicates whether the call of writeResultset has no side effect and can be retried to correct error. The call
// has side effect in cursor mode or once data has been sent to client. Currently retryable is used to fallback to TiKV when
// TiFlash is down.
func (cc *clientConn) writeResultset(ctx context.Context, rs ResultSet, binary bool, serverStatus uint16, fetchSize int) (retryable bool, runErr error) {
	defer func() {
		// close ResultSet when cursor doesn't exist
		r := recover()
		if r == nil {
			return
		}
		if str, ok := r.(string); !ok || !strings.HasPrefix(str, memory.PanicMemoryExceed) {
			panic(r)
		}
		// TODO(jianzhang.zj: add metrics here)
		runErr = errors.Errorf("%v", r)
		buf := make([]byte, 4096)
		stackSize := runtime.Stack(buf, false)
		buf = buf[:stackSize]
		logutil.Logger(ctx).Error("write query result panic", zap.Stringer("lastSQL", getLastStmtInConn{cc}), zap.String("stack", string(buf)))
	}()
	cc.initResultEncoder(ctx)
	defer cc.rsEncoder.clean()
	if mysql.HasCursorExistsFlag(serverStatus) {
		if err := cc.writeChunksWithFetchSize(ctx, rs, serverStatus, fetchSize); err != nil {
			return false, err
		}
		return false, cc.flush(ctx)
	}
	if retryable, err := cc.writeChunks(ctx, rs, binary, serverStatus); err != nil {
		return retryable, err
	}

	return false, cc.flush(ctx)
}

func (cc *clientConn) writeColumnInfo(columns []*ColumnInfo, serverStatus uint16) error {
	data := cc.alloc.AllocWithLen(4, 1024)
	data = dumpLengthEncodedInt(data, uint64(len(columns)))
	if err := cc.writePacket(data); err != nil {
		return err
	}
	for _, v := range columns {
		data = data[0:4]
		data = v.Dump(data, cc.rsEncoder)
		if err := cc.writePacket(data); err != nil {
			return err
		}
	}
	return cc.writeEOF(serverStatus)
}

// writeChunks writes data from a Chunk, which filled data by a ResultSet, into a connection.
// binary specifies the way to dump data. It throws any error while dumping data.
// serverStatus, a flag bit represents server information
// The first return value indicates whether error occurs at the first call of ResultSet.Next.
func (cc *clientConn) writeChunks(ctx context.Context, rs ResultSet, binary bool, serverStatus uint16) (bool, error) {
	data := cc.alloc.AllocWithLen(4, 1024)
	req := rs.NewChunk(cc.chunkAlloc)
	gotColumnInfo := false
	firstNext := true
	var stmtDetail *execdetails.StmtExecDetails
	stmtDetailRaw := ctx.Value(execdetails.StmtExecDetailKey)
	if stmtDetailRaw != nil {
		stmtDetail = stmtDetailRaw.(*execdetails.StmtExecDetails)
	}
	for {
		failpoint.Inject("fetchNextErr", func(value failpoint.Value) {
			switch value.(string) {
			case "firstNext":
				failpoint.Return(firstNext, storeerr.ErrTiFlashServerTimeout)
			case "secondNext":
				if !firstNext {
					failpoint.Return(firstNext, storeerr.ErrTiFlashServerTimeout)
				}
			}
		})
		// Here server.tidbResultSet implements Next method.
		err := rs.Next(ctx, req)
		if err != nil {
			return firstNext, err
		}
		firstNext = false
		if !gotColumnInfo {
			// We need to call Next before we get columns.
			// Otherwise, we will get incorrect columns info.
			columns := rs.Columns()
			if err = cc.writeColumnInfo(columns, serverStatus); err != nil {
				return false, err
			}
			gotColumnInfo = true
		}
		rowCount := req.NumRows()
		if rowCount == 0 {
			break
		}
		reg := trace.StartRegion(ctx, "WriteClientConn")
		start := time.Now()
		for i := 0; i < rowCount; i++ {
			data = data[0:4]
			if binary {
				data, err = dumpBinaryRow(data, rs.Columns(), req.GetRow(i), cc.rsEncoder)
			} else {
				data, err = dumpTextRow(data, rs.Columns(), req.GetRow(i), cc.rsEncoder)
			}
			if err != nil {
				reg.End()
				return false, err
			}
			if err = cc.writePacket(data); err != nil {
				reg.End()
				return false, err
			}
		}
		reg.End()
		if stmtDetail != nil {
			stmtDetail.WriteSQLRespDuration += time.Since(start)
		}
	}
	return false, cc.writeEOF(serverStatus)
}

// writeChunksWithFetchSize writes data from a Chunk, which filled data by a ResultSet, into a connection.
// binary specifies the way to dump data. It throws any error while dumping data.
// serverStatus, a flag bit represents server information.
// fetchSize, the desired number of rows to be fetched each time when client uses cursor.
func (cc *clientConn) writeChunksWithFetchSize(ctx context.Context, rs ResultSet, serverStatus uint16, fetchSize int) error {
	fetchedRows := rs.GetFetchedRows()
	for len(fetchedRows) < fetchSize {
		// if fetchedRows is not enough, getting data from recordSet.
		req := rs.NewChunk(cc.chunkAlloc)
		// Here server.tidbResultSet implements Next method.
		if err := rs.Next(ctx, req); err != nil {
			return err
		}
		rowCount := req.NumRows()
		if rowCount == 0 {
			break
		}
		// filling fetchedRows with chunk
		for i := 0; i < rowCount; i++ {
			fetchedRows = append(fetchedRows, req.GetRow(i))
		}
		req = chunk.Renew(req, cc.ctx.GetSessionVars().MaxChunkSize)
	}

	// tell the client COM_STMT_FETCH has finished by setting proper serverStatus,
	// and close ResultSet.
	if len(fetchedRows) == 0 {
		serverStatus &^= mysql.ServerStatusCursorExists
		serverStatus |= mysql.ServerStatusLastRowSend
		terror.Call(rs.Close)
		return cc.writeEOF(serverStatus)
	}

	// construct the rows sent to the client according to fetchSize.
	var curRows []chunk.Row
	if fetchSize < len(fetchedRows) {
		curRows = fetchedRows[:fetchSize]
		fetchedRows = fetchedRows[fetchSize:]
	} else {
		curRows = fetchedRows
		fetchedRows = fetchedRows[:0]
	}
	rs.StoreFetchedRows(fetchedRows)

	data := cc.alloc.AllocWithLen(4, 1024)
	var stmtDetail *execdetails.StmtExecDetails
	stmtDetailRaw := ctx.Value(execdetails.StmtExecDetailKey)
	if stmtDetailRaw != nil {
		stmtDetail = stmtDetailRaw.(*execdetails.StmtExecDetails)
	}
	start := time.Now()
	var err error
	for _, row := range curRows {
		data = data[0:4]
		data, err = dumpBinaryRow(data, rs.Columns(), row, cc.rsEncoder)
		if err != nil {
			return err
		}
		if err = cc.writePacket(data); err != nil {
			return err
		}
	}
	if stmtDetail != nil {
		stmtDetail.WriteSQLRespDuration += time.Since(start)
	}
	if cl, ok := rs.(fetchNotifier); ok {
		cl.OnFetchReturned()
	}
	return cc.writeEOF(serverStatus)
}

func (cc *clientConn) setConn(conn net.Conn) {
	cc.bufReadConn = newBufferedReadConn(conn)
	if cc.pkt == nil {
		cc.pkt = newPacketIO(cc.bufReadConn)
	} else {
		// Preserve current sequence number.
		cc.pkt.setBufferedReadConn(cc.bufReadConn)
	}
}

func (cc *clientConn) upgradeToTLS(tlsConfig *tls.Config) error {
	// Important: read from buffered reader instead of the original net.Conn because it may contain data we need.
	tlsConn := tls.Server(cc.bufReadConn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	cc.setConn(tlsConn)
	cc.tlsConn = tlsConn
	return nil
}

func (cc *clientConn) handleChangeUser(ctx context.Context, data []byte) error {
	user, data := parseNullTermString(data)
	cc.user = string(hack.String(user))
	if len(data) < 1 {
		return mysql.ErrMalformPacket
	}
	passLen := int(data[0])
	data = data[1:]
	if passLen > len(data) {
		return mysql.ErrMalformPacket
	}
	pass := data[:passLen]
	data = data[passLen:]
	dbName, _ := parseNullTermString(data)
	cc.dbname = string(hack.String(dbName))

	if err := cc.ctx.Close(); err != nil {
		logutil.Logger(ctx).Debug("close old context failed", zap.Error(err))
	}
	if err := cc.openSessionAndDoAuth(pass, ""); err != nil {
		return err
	}
	return cc.handleCommonConnectionReset(ctx)
}

func (cc *clientConn) handleResetConnection(ctx context.Context) error {
	user := cc.ctx.GetSessionVars().User
	err := cc.ctx.Close()
	if err != nil {
		logutil.Logger(ctx).Debug("close old context failed", zap.Error(err))
	}
	var tlsStatePtr *tls.ConnectionState
	if cc.tlsConn != nil {
		tlsState := cc.tlsConn.ConnectionState()
		tlsStatePtr = &tlsState
	}
	cc.ctx, err = cc.server.driver.OpenCtx(cc.connectionID, cc.capability, cc.collation, cc.dbname, tlsStatePtr)
	if err != nil {
		return err
	}
	if !cc.ctx.AuthWithoutVerification(user) {
		return errors.New("Could not reset connection")
	}
	if cc.dbname != "" { // Restore the current DB
		err = cc.useDB(context.Background(), cc.dbname)
		if err != nil {
			return err
		}
	}
	cc.ctx.SetSessionManager(cc.server)

	return cc.handleCommonConnectionReset(ctx)
}

func (cc *clientConn) handleCommonConnectionReset(ctx context.Context) error {
	if plugin.IsEnable(plugin.Audit) {
		cc.ctx.GetSessionVars().ConnectionInfo = cc.connectInfo()
	}

	err := plugin.ForeachPlugin(plugin.Audit, func(p *plugin.Plugin) error {
		authPlugin := plugin.DeclareAuditManifest(p.Manifest)
		if authPlugin.OnConnectionEvent != nil {
			connInfo := cc.ctx.GetSessionVars().ConnectionInfo
			err := authPlugin.OnConnectionEvent(context.Background(), plugin.ChangeUser, connInfo)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return cc.writeOK(ctx)
}

// safe to noop except 0x01 "FLUSH PRIVILEGES"
func (cc *clientConn) handleRefresh(ctx context.Context, subCommand byte) error {
	if subCommand == 0x01 {
		if err := cc.handleQuery(ctx, "FLUSH PRIVILEGES"); err != nil {
			return err
		}
	}
	return cc.writeOK(ctx)
}

var _ fmt.Stringer = getLastStmtInConn{}

type getLastStmtInConn struct {
	*clientConn
}

func (cc getLastStmtInConn) String() string {
	if len(cc.lastPacket) == 0 {
		return ""
	}
	cmd, data := cc.lastPacket[0], cc.lastPacket[1:]
	switch cmd {
	case mysql.ComInitDB:
		return "Use " + string(data)
	case mysql.ComFieldList:
		return "ListFields " + string(data)
	case mysql.ComQuery, mysql.ComStmtPrepare:
		sql := string(hack.String(data))
		if cc.ctx.GetSessionVars().EnableRedactLog {
			sql = parser.Normalize(sql)
		}
		return tidbutil.QueryStrForLog(sql)
	case mysql.ComStmtExecute, mysql.ComStmtFetch:
		stmtID := binary.LittleEndian.Uint32(data[0:4])
		return tidbutil.QueryStrForLog(cc.preparedStmt2String(stmtID))
	case mysql.ComStmtClose, mysql.ComStmtReset:
		stmtID := binary.LittleEndian.Uint32(data[0:4])
		return mysql.Command2Str[cmd] + " " + strconv.Itoa(int(stmtID))
	default:
		if cmdStr, ok := mysql.Command2Str[cmd]; ok {
			return cmdStr
		}
		return string(hack.String(data))
	}
}

// PProfLabel return sql label used to tag pprof.
func (cc getLastStmtInConn) PProfLabel() string {
	if len(cc.lastPacket) == 0 {
		return ""
	}
	cmd, data := cc.lastPacket[0], cc.lastPacket[1:]
	switch cmd {
	case mysql.ComInitDB:
		return "UseDB"
	case mysql.ComFieldList:
		return "ListFields"
	case mysql.ComStmtClose:
		return "CloseStmt"
	case mysql.ComStmtReset:
		return "ResetStmt"
	case mysql.ComQuery, mysql.ComStmtPrepare:
		return parser.Normalize(tidbutil.QueryStrForLog(string(hack.String(data))))
	case mysql.ComStmtExecute, mysql.ComStmtFetch:
		stmtID := binary.LittleEndian.Uint32(data[0:4])
		return tidbutil.QueryStrForLog(cc.preparedStmt2StringNoArgs(stmtID))
	default:
		return ""
	}
}
