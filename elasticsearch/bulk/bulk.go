package bulk

import (
	"bytes"
	"sync"
	"time"

	"github.com/Trendyol/go-dcp-client/models"
	"github.com/Trendyol/go-elasticsearch-connect-couchbase/config"
	"github.com/Trendyol/go-elasticsearch-connect-couchbase/elasticsearch/client"
	"github.com/Trendyol/go-elasticsearch-connect-couchbase/elasticsearch/document"
	"github.com/Trendyol/go-elasticsearch-connect-couchbase/helper"
	"github.com/Trendyol/go-elasticsearch-connect-couchbase/logger"
	"github.com/elastic/go-elasticsearch/v7"
)

type Bulk struct {
	errorLogger            logger.Logger
	logger                 logger.Logger
	dcpCheckpointCommit    func()
	batchTicker            *time.Ticker
	isClosed               chan bool
	actionCh               chan document.ESActionDocument
	esClient               *elasticsearch.Client
	metric                 *Metric
	collectionIndexMapping map[string]string
	typeName               []byte
	batch                  []byte
	batchSize              int
	batchSizeLimit         int
	batchTickerDuration    time.Duration
	flushLock              sync.Mutex
	batchByteSizeLimit     int
}

type Metric struct {
	ESConnectorLatency int64
}

func NewBulk(
	config *config.Config,
	logger logger.Logger,
	errorLogger logger.Logger,
	dcpCheckpointCommit func(),
) (*Bulk, error) {
	esClient, err := client.NewElasticClient(config)
	if err != nil {
		return nil, err
	}

	bulk := &Bulk{
		batchTickerDuration:    config.Elasticsearch.BatchTickerDuration,
		batchTicker:            time.NewTicker(config.Elasticsearch.BatchTickerDuration),
		actionCh:               make(chan document.ESActionDocument, config.Elasticsearch.BatchSizeLimit),
		batchSizeLimit:         config.Elasticsearch.BatchSizeLimit,
		batchByteSizeLimit:     config.Elasticsearch.BatchByteSizeLimit,
		isClosed:               make(chan bool, 1),
		logger:                 logger,
		errorLogger:            errorLogger,
		dcpCheckpointCommit:    dcpCheckpointCommit,
		esClient:               esClient,
		metric:                 &Metric{},
		collectionIndexMapping: config.Elasticsearch.CollectionIndexMapping,
		typeName:               helper.Byte(config.Elasticsearch.TypeName),
	}

	go bulk.StartBulk()
	return bulk, nil
}

func (b *Bulk) StartBulk() {
	for range b.batchTicker.C {
		if len(b.batch) == 0 {
			continue
		}
		err := b.flushMessages()
		if err != nil {
			b.errorLogger.Printf("Batch producer flush error %v", err)
		}
	}
}

func (b *Bulk) AddAction(
	ctx *models.ListenerContext,
	eventTime time.Time,
	action document.ESActionDocument,
	collectionName string,
) {
	b.flushLock.Lock()
	b.batch = append(
		b.batch,
		getEsActionJSON(
			action.ID,
			action.Type,
			b.collectionIndexMapping[collectionName],
			action.Routing,
			action.Source,
			b.typeName,
		)...,
	)
	b.batchSize++
	ctx.Ack()
	b.flushLock.Unlock()

	b.metric.ESConnectorLatency = time.Since(eventTime).Milliseconds()

	if b.batchSize == b.batchSizeLimit || len(b.batch) >= b.batchByteSizeLimit {
		err := b.flushMessages()
		if err != nil {
			b.errorLogger.Printf("Bulk writer error %v", err)
		}
	}
}

var (
	indexPrefix   = helper.Byte(`{"index":{"_index":"`)
	deletePrefix  = helper.Byte(`{"delete":{"_index":"`)
	idPrefix      = helper.Byte(`","_id":"`)
	typePrefix    = helper.Byte(`","_type":"`)
	routingPrefix = helper.Byte(`","_routing":"`)
	postFix       = helper.Byte(`"}}`)
)

func getEsActionJSON(docID []byte, action document.EsAction, indexName string, routing *string, source []byte, typeName []byte) []byte {
	var meta []byte
	if action == document.Index {
		meta = indexPrefix
	} else {
		meta = deletePrefix
	}
	meta = append(meta, helper.Byte(indexName)...)
	meta = append(meta, idPrefix...)
	meta = append(meta, docID...)
	if routing != nil {
		meta = append(meta, routingPrefix...)
		meta = append(meta, helper.Byte(*routing)...)
	}
	meta = append(meta, typePrefix...)
	meta = append(meta, typeName...)
	meta = append(meta, postFix...)
	if action == document.Index {
		meta = append(meta, '\n')
		meta = append(meta, source...)
	}
	meta = append(meta, '\n')
	return meta
}

func (b *Bulk) Close() {
	b.batchTicker.Stop()

	err := b.flushMessages()
	if err != nil {
		b.errorLogger.Printf("Bulk error %v", err)
	}
}

func (b *Bulk) flushMessages() error {
	b.flushLock.Lock()
	defer b.flushLock.Unlock()

	err := b.bulkRequest()
	if err != nil {
		return err
	}

	b.batchTicker.Reset(b.batchTickerDuration)
	b.batch = b.batch[:0]
	b.batchSize = 0
	b.dcpCheckpointCommit()
	return nil
}

func (b *Bulk) bulkRequest() error {
	reader := bytes.NewReader(b.batch)
	_, err := b.esClient.Bulk(reader)
	if err != nil {
		return err
	}
	return nil
}

func (b *Bulk) GetMetric() *Metric {
	return b.metric
}
