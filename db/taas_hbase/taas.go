package taas_hbase

import (
	"fmt"
	"github.com/apache/thrift/lib/go/thrift"
	"github.com/pingcap/errors"
	"github.com/pingcap/go-ycsb/db/taas"
	"log"
	"net"
	"reflect"
	"sync/atomic"
	"time"
	"unsafe"
)

import (
	"context"
	"github.com/golang/protobuf/proto"
	"github.com/pingcap/go-ycsb/db/taas_proto"
)

func (db *txnDB) TxnCommit(ctx context.Context, table string, keys []string, values []map[string][]byte) error {
	for taas.InitOk == 0 {
		time.Sleep(50)
	}

	t1 := time.Now().UnixNano()
	txnId := atomic.AddUint64(&taas.CSNCounter, 1) // return new value
	atomic.AddUint64(&taas.TotalTransactionCounter, 1)
	txnSendToTaas := taas_proto.Transaction{
		StartEpoch:  0,
		CommitEpoch: 5,
		Csn:         uint64(time.Now().UnixNano()),
		ClientIp:    taas.LocalServerIp,
		ClientTxnId: txnId,
		TxnType:     taas_proto.TxnType_ClientTxn,
		TxnState:    0,
		StorageType: "kv",
	}

	protocolFactory := thrift.NewTBinaryProtocolFactoryConf(&thrift.TConfiguration{
		TBinaryStrictRead:  thrift.BoolPtr(true),
		TBinaryStrictWrite: thrift.BoolPtr(true),
	})
	transport := thrift.NewTSocketConf(net.JoinHostPort(taas.HbaseServerIp, PORT), &thrift.TConfiguration{
		ConnectTimeout: time.Second * 5,
		SocketTimeout:  time.Second * 5,
	})
	client := NewTHBaseServiceClientFactory(transport, protocolFactory)
	err := transport.Open()
	if err != nil {
		return err
	}
	defer transport.Close()

	var readOpNum, writeOpNum uint64 = 0, 0
	time1 := time.Now()
	for i, key := range keys {
		if values[i] == nil { //read
			readOpNum++
			rowKey := db.getRowKey(table, key)
			time2 := time.Now()
			rowData, err := client.Get(ctx, []byte(table), &TGet{Row: []byte(rowKey)})
			if err != nil {
				return err
			} else if rowData == nil {
				return errors.New("txn read failed")
			}
			res := make(map[string][]byte)
			for _, column := range rowData.ColumnValues {
				c := reflect.ValueOf(column).Elem()
				family := c.Field(0)
				value := c.Field(2)
				res[string(family.Interface().([]uint8))] = value.Interface().([]byte)
			}
			timeLen2 := time.Now().Sub(time2)
			atomic.AddUint64(&taas.TikvReadLatency, uint64(timeLen2))
			if err != nil {
				return err
			}
			sendRow := taas_proto.Row{
				OpType: taas_proto.OpType_Read,
				Key:    *(*[]byte)(unsafe.Pointer(&rowKey)),
				Data:   []byte(res["entire"]),
				Csn:    0,
			}
			txnSendToTaas.Row = append(txnSendToTaas.Row, &sendRow)
			//fmt.Println("; Read, key : " + string(rowKey) + " Data : " + string(rowData))
		} else {
			writeOpNum++
			rowKey := db.getRowKey(table, key)
			rowData, err := db.r.Encode(nil, values[i])
			if err != nil {
				return err
			}
			sendRow := taas_proto.Row{
				OpType: taas_proto.OpType_Update,
				Key:    *(*[]byte)(unsafe.Pointer(&rowKey)),
				Data:   []byte(rowData),
			}
			txnSendToTaas.Row = append(txnSendToTaas.Row, &sendRow)
		}

	}

	timeLen := time.Now().Sub(time1)
	atomic.AddUint64(&taas.TikvTotalLatency, uint64(timeLen))
	//fmt.Println("; read op : " + strconv.FormatUint(readOpNum, 10) + ", write op : " + strconv.FormatUint(writeOpNum, 10))

	sendMessage := &taas_proto.Message{
		Type: &taas_proto.Message_Txn{Txn: &txnSendToTaas},
	}
	sendBuffer, err := proto.Marshal(sendMessage)
	if err != nil {
		return err
	}
	sendString, err := taas.GzipBytes(sendBuffer)
	if err != nil {
		return err
	}
	taas.TaasTxnCH <- taas.TaasTxn{GzipedTransaction: sendString}

	result, ok := <-(taas.ChanList[txnId%uint64(taas.ClientNum)])
	//fmt.Println("Receive From Taas")
	t2 := uint64(time.Now().UnixNano() - t1)
	taas.TotalLatency += t2
	//append(latency, t2)
	//result, ok := "Abort", true
	atomic.AddUint64(&taas.TotalReadCounter, uint64(readOpNum))
	atomic.AddUint64(&taas.TotalUpdateCounter, uint64(writeOpNum))
	if ok {
		if result != "Commit" {
			atomic.AddUint64(&taas.FailedReadCounter, uint64(readOpNum))
			atomic.AddUint64(&taas.FailedUpdateounter, uint64(writeOpNum))
			atomic.AddUint64(&taas.FailedTransactionCounter, 1)
			atomic.AddUint64(&taas.TotalFailedLatency, t2)
			//fmt.Println("Commit Failed")
			return errors.New("txn conflict handle failed")
		}
		atomic.AddUint64(&taas.SuccessReadCounter, uint64(readOpNum))
		atomic.AddUint64(&taas.SuccessUpdateCounter, uint64(writeOpNum))
		atomic.AddUint64(&taas.SuccessTransactionCounter, 1)
		atomic.AddUint64(&taas.TotalSuccessLatency, t2)
		//fmt.Println("Commit Success")
	} else {
		fmt.Println("txn_bak.go 481")
		log.Fatal(ok)
		return err
	}
	return nil
}
