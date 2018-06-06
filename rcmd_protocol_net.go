package rcmd

import (
	"github.com/golang/glog"
	"github.com/apache/incubator-rocketmq-externals/rocketmq-go/util"

	"net"
	"time"
	"strconv"
	"fmt"
	"bytes"
	"encoding/binary"
	"github.com/getlantern/errors"
)

type InvokeCallback func(responseFuture *ResponseFuture)

type ProtocolNet struct {
	responseTable  util.ConcurrentMap
	cmdHandleTable util.ConcurrentMap
}

type ResponseFuture struct {
	ResponseProtocol *Protocol
	Success           bool
	Error             error
	Seq               int32
	TimeoutMillis     int64
	InvokeCallback    InvokeCallback
	BeginTimestamp    int64
	Done               chan bool
}

func (pn *ProtocolNet)SendSync(conn net.Conn, protocol Protocol, timeoutMillis int64) (*Protocol, error) {
	responseFuture := NewDefaultResponseFuture(
		protocol.ProtocolHeader.Seq,
		timeoutMillis,
		nil,
	)
	pn.setResponse(protocol.ProtocolHeader.Seq, responseFuture)
	err := pn.sendProtocol(conn, protocol)
	if err != nil {
		glog.Error(err)
		return  nil,err
	}

	select {
		case <-responseFuture.Done:
			return responseFuture.ResponseProtocol, nil
		case <-time.After(time.Duration(timeoutMillis) * time.Millisecond):
			return nil, fmt.Errorf("SendSync timeout: %d Millisecond", timeoutMillis)
	}
}

func (pn *ProtocolNet)SendAsync(conn net.Conn, protocol Protocol, callback InvokeCallback, timeoutMillis int64) error {
	responseFuture := NewDefaultResponseFuture(
		protocol.ProtocolHeader.Seq,
		timeoutMillis,
		callback,
	)
	pn.setResponse(protocol.ProtocolHeader.Seq, responseFuture)
	err := pn.sendProtocol(conn, protocol)
	if err != nil {
		glog.Error(err)
	}

	return err
}

func (pn *ProtocolNet)SendOneWay(conn net.Conn, protocol Protocol) error  {
	protocol.ProtocolHeader.SetFlag(OneWayFlag)
	err := pn.sendProtocol(conn, protocol)
	if err != nil {
		glog.Error(err)
	}

	return err
}

func (pn *ProtocolNet)sendProtocol(conn net.Conn, protocol Protocol) error {
	buf,err := protocol.ToBytes()
	if err != nil {
		return err
	}

	_, err = conn.Write(buf)
	if err != nil {
		return err
	}

	return nil
}

func (pn *ProtocolNet)ReceiveLoop(conn net.Conn) error {

	buf := make([]byte, 1024)

	needRedSize := true
	protocolBuf := bytes.NewBuffer([]byte{})
	var size, flag, seq int32
	var cmdCode, version int16
	for  {

		n, err := conn.Read(buf)
		if err != nil {
			return err
		}

		_, err = protocolBuf.Write(buf[:n])
		if err != nil {
			return err
		}

		for {

			if needRedSize {

				if protocolBuf.Len() >= 4 {
					err = binary.Read(protocolBuf, binary.BigEndian, &size)
					if err != nil {
						return err
					}

					needRedSize = false
				}

			} else {
				break
			}

			if !needRedSize {

				if protocolBuf.Len() < int(size) {
					break;
				}

			}

			//读取协议头
			binary.Read(protocolBuf, binary.BigEndian, &size)
			binary.Read(protocolBuf, binary.BigEndian, &flag)
			binary.Read(protocolBuf, binary.BigEndian, &cmdCode)
			binary.Read(protocolBuf, binary.BigEndian, &version)
			binary.Read(protocolBuf, binary.BigEndian, &seq)

			//读取协议头后剩下的就是协议体
			var bodyBuf []byte  = nil
			if protocolBuf.Len() > 0 {
				binary.Read(protocolBuf, binary.BigEndian, bodyBuf)
			}

			protocolHeader := NewProtocolHeader(flag, version, cmdCode, seq)
			protocol := NewProtocol(protocolHeader, bodyBuf)

			go pn.messageLoop(protocol, conn)

		}

	}

}

func (pn *ProtocolNet)messageLoop(protocol *Protocol, conn net.Conn) {
	if protocol.ProtocolHeader.IsResponse() {

		pn.responseHandle(protocol, conn)

	} else if protocol.ProtocolHeader.IsRequest() {

		pn.requestHandle(protocol, conn)

	}
}

func (pn *ProtocolNet)requestHandle(protocol *Protocol, conn net.Conn) {

	//oneWay消息不需要回复响应
	if !protocol.ProtocolHeader.IsOneWay() {

	}

}

func (pn *ProtocolNet)responseHandle(protocol *Protocol, conn net.Conn) {

	responseFuture, err := pn.getResponse(protocol.ProtocolHeader.Seq)
	if err != nil {
		glog.Error(err)
		return
	}

	pn.removeResponse(protocol.ProtocolHeader.Seq)

	responseFuture.ToSucceedAndExecuteCallback(protocol)

}

func (pn *ProtocolNet)getResponse(seq int32) (*ResponseFuture, error) {
	data, result := pn.responseTable.Get(strconv.Itoa(int(seq)))
	if !result {
		return nil, errors.New("find response future fail:not found.")
	}

	return data.(*ResponseFuture), nil
}

func (pn *ProtocolNet)setResponse(seq int32, future *ResponseFuture) {
	pn.responseTable.Set(strconv.Itoa(int(seq)), future)
}

func (pn *ProtocolNet)removeResponse(seq int32) {
	pn.responseTable.Remove(strconv.Itoa(int(seq)))
}

func NewDefaultResponseFuture(seq int32, timeoutMillis int64, invokeCallback InvokeCallback) *ResponseFuture  {
	return &ResponseFuture{
		ResponseProtocol:nil,
		Success:false,
		Error:nil,
		Seq:seq,
		TimeoutMillis:timeoutMillis,
		InvokeCallback:invokeCallback,
		BeginTimestamp: time.Now().Unix(),
		Done:make(chan bool),
	}
}

func (rf *ResponseFuture) IsSucceed() bool {
	return  rf.Success
}

func (rf *ResponseFuture) IsFailed() bool  {
	return rf.Error != nil
}

func (rf *ResponseFuture) ExecuteCallback() {
	if rf.InvokeCallback != nil {
		rf.InvokeCallback(rf)
	}
}

func (rf *ResponseFuture)ToSucceed(responseProtocol *Protocol) {
	rf.ResponseProtocol = responseProtocol
	rf.Success = true
	rf.Error = nil
	close(rf.Done)
}

func (rf *ResponseFuture)ToSucceedAndExecuteCallback(responseProtocol *Protocol) {
	rf.ToSucceed(responseProtocol)
	rf.ExecuteCallback()
}

func (rf *ResponseFuture)ToFailed(responseProtocol *Protocol, err error)  {
	rf.ResponseProtocol = nil
	rf.Success = false
	rf.Error = err
}

func (rf *ResponseFuture)ToFailedAndExecuteCallback(responseProtocol *Protocol, err error)  {
	rf.ToFailed(responseProtocol, err)
	rf.ExecuteCallback()
}