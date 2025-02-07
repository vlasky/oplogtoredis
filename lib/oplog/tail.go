// Package oplog tails a MongoDB oplog, process each message, and generates
// the message that should be sent to Redis. It writes these to an output
// channel that should be read by package redispub and sent to the Redis server.
package oplog

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/vlasky/oplogtoredis/lib/config"
	"github.com/vlasky/oplogtoredis/lib/log"
	"github.com/vlasky/oplogtoredis/lib/redispub"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.mongodb.org/mongo-driver/bson"
)

// Tailer persistently tails the oplog of a Mongo cluster, handling
// reconnection and resumption of where it left off.
type Tailer struct {
	MongoClient *mongo.Client
	RedisClient redis.UniversalClient
	RedisPrefix string
	MaxCatchUp  time.Duration
}

// Raw oplog entry from Mongo
type rawOplogEntry struct {
	Timestamp    primitive.Timestamp `bson:"ts"`
	HistoryID    int64               `bson:"h"`
	MongoVersion int                 `bson:"v"`
	Operation    string              `bson:"op"`
	Namespace    string              `bson:"ns"`
	Doc          bson.Raw            `bson:"o"`
	Update       rawOplogEntryID     `bson:"o2"`
}

type rawOplogEntryID struct {
	ID interface{} `bson:"_id"`
}

const requeryDuration = time.Second

var (
	// Deprecated: use metricOplogEntriesBySize instead
	metricOplogEntriesReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "otr",
		Subsystem: "oplog",
		Name:      "entries_received",
		Help:      "[Deprecated] Oplog entries received, partitioned by database and status",
	}, []string{"database", "status"})

	// Deprecated: use metricOplogEntriesBySize instead
	metricOplogEntriesReceivedSize = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "otr",
		Subsystem: "oplog",
		Name:      "entries_received_size",
		Help:      "[Deprecated] Size of oplog entries received in bytes, partitioned by database",
	}, []string{"database"})

	metricOplogEntriesBySize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "otr",
		Subsystem: "oplog",
		Name:      "entries_by_size",
		Help:      "Histogram of oplog entries received by size in bytes, partitioned by database and status.",
		Buckets:   append([]float64{0}, prometheus.ExponentialBuckets(8, 2, 29)...),
	}, []string{"database", "status"})

	metricMaxOplogEntryByMinute = NewIntervalMaxMetricVec(&IntervalMaxVecOpts{
		IntervalMaxOpts: IntervalMaxOpts{
			Opts: prometheus.Opts{
				Namespace: "otr",
				Subsystem: "oplog",
				Name:      "entries_max_size",
				Help:      "Gauge recording maximum size recorded in the last minute, partitioned by database and status",
			},

			ReportInterval: 1 * time.Minute,
		},
	}, []string{"database", "status"})
)

func init() {
	prometheus.MustRegister(metricMaxOplogEntryByMinute)
}

// Tail begins tailing the oplog. It doesn't return unless it receives a message
// on the stop channel, in which case it wraps up its work and then returns.
func (tailer *Tailer) Tail(out chan<- *redispub.Publication, stop <-chan bool) {
	childStopC := make(chan bool)
	wasStopped := false

	go func() {
		<-stop
		wasStopped = true
		childStopC <- true
	}()

	for {
		log.Log.Info("Starting oplog tailing")
		tailer.tailOnce(out, childStopC)
		log.Log.Info("Oplog tailing ended")

		if wasStopped {
			return
		}

		log.Log.Errorw("Oplog tailing stopped prematurely. Waiting a second an then retrying.")
		time.Sleep(requeryDuration)
	}
}

func (tailer *Tailer) tailOnce(out chan<- *redispub.Publication, stop <-chan bool) {
	session, err := tailer.MongoClient.StartSession()
	if err != nil {
		log.Log.Errorw("Failed to start Mongo session", "error", err)
		return
	}

	oplogCollection := session.Client().Database("local").Collection("oplog.rs")

	startTime := tailer.getStartTime(func() (primitive.Timestamp, error) {
		// Get the timestamp of the last entry in the oplog (as a position to
		// start from if we don't have a last-written timestamp from Redis)
		var entry rawOplogEntry
		findOneOpts := &options.FindOneOptions{}
		findOneOpts.SetSort(bson.M{"$natural": -1})

		queryContext, queryContextCancel := context.WithTimeout(context.Background(), config.MongoQueryTimeout())
		defer queryContextCancel()

		result := oplogCollection.FindOne(queryContext, bson.M{}, findOneOpts)

		if result.Err() != nil {
			return entry.Timestamp, result.Err()
		}

		decodeErr := result.Decode(&entry)

		if decodeErr != nil {
			return entry.Timestamp, decodeErr
		}

		log.Log.Infow("Got latest oplog entry",
			"entry", entry)

		return entry.Timestamp, nil
	})

	query, queryErr := issueOplogFindQuery(oplogCollection, startTime)

	if queryErr != nil {
		log.Log.Errorw("Error issuing tail query", "error", queryErr)
		return
	}

	lastTimestamp := startTime
	for {
		select {
		case <-stop:
			log.Log.Infof("Received stop; aborting oplog tailing")
			return
		default:
		}

		var rawData bson.Raw

		for {
			gotResult, didTimeout, didLosePosition, err := readNextFromCursor(query)

			if gotResult {
				decodeErr := query.Decode(&rawData)
				if decodeErr != nil {
					log.Log.Errorw("Error decoding oplog entry", "error", decodeErr)

				}

				ts, pubs := tailer.unmarshalEntry(rawData)

				if ts != nil {
					lastTimestamp = *ts
				}

				for _, pub := range pubs {
					if pub != nil {
						out <- pub
					} else {
						log.Log.Error("Nil Redis publication")
					}
				}
			} else if didTimeout {
				log.Log.Info("Oplog cursor timed out, will retry")

				query, queryErr = issueOplogFindQuery(oplogCollection, lastTimestamp)

				if queryErr != nil {
					log.Log.Errorw("Error issuing tail query", "error", queryErr)
					return
				}

				break
			} else if didLosePosition {
				// Our cursor expired. Make a new cursor to pick up from where we
				// left off.
				query, queryErr = issueOplogFindQuery(oplogCollection, lastTimestamp)

				if queryErr != nil {
					log.Log.Errorw("Error issuing tail query", "error", queryErr)
					return
				}

				break
			} else if err != nil {
				log.Log.Errorw("Error from oplog iterator",
					"error", query.Err())

				closeCursor(query)

				return
			} else {
				log.Log.Errorw("Got no data from cursor, but also no error. This is unexpected; restarting query")

				closeCursor(query)

				return
			}
		}
	}
}

func readNextFromCursor(cursor *mongo.Cursor) (gotResult bool, didTimeout bool, didLosePosition bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.MongoQueryTimeout())
	defer cancel()

	gotResult = cursor.Next(ctx)
	err = cursor.Err()

	if err != nil {
		// Wait briefly before determining whether the failure was a timeout: because
		// the MongoDB go driver passes the context on to lower-level networking
		// components, it's possible for the query to fail *before* the context
		// is marked as timed-out
		time.Sleep(100 * time.Millisecond)
		didTimeout = ctx.Err() != nil

		// check if the error is a position-lost error. These errors are best handled
		// by just re-issueing the query on the same connection; no need to surface
		// a major error and re-connect to mongo
		// From: https://github.com/rwynn/gtm/blob/e02a1f9c1b79eb5f14ed26c86a23b920589d84c9/gtm.go#L547
		var serverErr mongo.ServerError
		if errors.As(err, &serverErr) {
			// 136  : cursor capped position lost
			// 286  : change stream history lost
			// 280  : change stream fatal error
			for _, code := range []int{136, 286, 280} {
				if serverErr.HasErrorCode(code) {
					didLosePosition = true
				}
			}
		}

	}

	return
}

func issueOplogFindQuery(c *mongo.Collection, startTime primitive.Timestamp) (*mongo.Cursor, error) {
	queryOpts := &options.FindOptions{}
	queryOpts.SetSort(bson.M{"$natural": 1})
	queryOpts.SetCursorType(options.TailableAwait)

	queryContext, queryContextCancel := context.WithTimeout(context.Background(), config.MongoQueryTimeout())
	defer queryContextCancel()

	return c.Find(queryContext, bson.M{
		"ts": bson.M{"$gt": startTime},
	}, queryOpts)
}

func closeCursor(cursor *mongo.Cursor) {
	queryContext, queryContextCancel := context.WithTimeout(context.Background(), config.MongoQueryTimeout())
	defer queryContextCancel()

	closeErr := cursor.Close(queryContext)
	if closeErr != nil {
		log.Log.Errorw("Error from closing oplog iterator",
			"error", closeErr)
	}
}

// unmarshalEntry unmarshals a single entry from the oplog.
//
// The timestamp of the entry is returned so that tailOnce knows the timestamp of the last entry it read, even if it
// ignored it or failed at some later step.
func (tailer *Tailer) unmarshalEntry(rawData bson.Raw) (timestamp *primitive.Timestamp, pubs []*redispub.Publication) {
	var result rawOplogEntry

	err := bson.Unmarshal(rawData, &result)
	if err != nil {
		log.Log.Errorw("Error unmarshalling oplog entry", "error", err)
		return
	}

	timestamp = &result.Timestamp

	entries := tailer.parseRawOplogEntry(result, nil)
	log.Log.Debugw("Received oplog entry",
		"entry", result)

	status := "ignored"
	database := "(no database)"
	messageLen := float64(len(rawData))

	defer func() {
		// TODO: remove these in a future version
		metricOplogEntriesReceived.WithLabelValues(database, status).Inc()
		metricOplogEntriesReceivedSize.WithLabelValues(database).Add(messageLen)

		metricOplogEntriesBySize.WithLabelValues(database, status).Observe(messageLen)
		metricMaxOplogEntryByMinute.Report(messageLen, database, status)
	}()

	if len(entries) > 0 {
		database = entries[0].Database
	}

	type errEntry struct {
		err error
		op  *oplogEntry
	}

	var errs []errEntry
	for i := range entries {
		entry := &entries[i]
		pub, err := processOplogEntry(entry)

		if err != nil {
			errs = append(errs, errEntry{
				err: err,
				op:  entry,
			})
		} else if pub != nil {
			pubs = append(pubs, pub)
		}
	}

	if errs != nil {
		status = "error"

		for _, ent := range errs {
			log.Log.Errorw("Error processing oplog entry",
				"op", ent.op,
				"error", ent.err,
				"database", ent.op.Database,
				"collection", ent.op.Database,
			)
		}
	} else if len(entries) > 0 {
		status = "processed"
	}

	return
}

// Gets the primitive.Timestamp from which we should start tailing
//
// We take the function to get the timestamp of the last oplog entry (as a
// fallback if we don't have a latest timestamp from Redis) as an arg instead
// of using tailer.mongoClient directly so we can unit test this function
func (tailer *Tailer) getStartTime(getTimestampOfLastOplogEntry func() (primitive.Timestamp, error)) primitive.Timestamp {
	ts, tsTime, redisErr := redispub.LastProcessedTimestamp(tailer.RedisClient, tailer.RedisPrefix)

	if redisErr == nil {
		// we have a last write time, check that it's not too far in the
		// past
		if tsTime.After(time.Now().Add(-1 * tailer.MaxCatchUp)) {
			log.Log.Infof("Found last processed timestamp, resuming oplog tailing from %d", tsTime.Unix())
			return ts
		}

		log.Log.Warnf("Found last processed timestamp, but it was too far in the past (%d). Will start from end of oplog", tsTime.Unix())
	}

	if (redisErr != nil) && (redisErr != redis.Nil) {
		log.Log.Errorw("Error querying Redis for last processed timestamp. Will start from end of oplog.",
			"error", redisErr)
	}

	mongoOplogEndTimestamp, mongoErr := getTimestampOfLastOplogEntry()
	if mongoErr == nil {
		log.Log.Infof("Starting tailing from end of oplog (timestamp %d)", mongoOplogEndTimestamp.T)
		return mongoOplogEndTimestamp
	}

	log.Log.Errorw("Got error when asking for last operation timestamp in the oplog. Returning current time.",
		"error", mongoErr)
	return primitive.Timestamp{T: uint32(time.Now().Unix() << 32)}
}

// converts a rawOplogEntry to an oplogEntry
func (tailer *Tailer) parseRawOplogEntry(entry rawOplogEntry, txIdx *uint) []oplogEntry {
	if txIdx == nil {
		idx := uint(0)
		txIdx = &idx
	}

	switch entry.Operation {
	case operationInsert, operationUpdate, operationRemove:
		var data map[string]interface{}
		if err := bson.Unmarshal(entry.Doc, &data); err != nil {
			log.Log.Errorf("unmarshalling oplog entry data: %v", err)
			return nil
		}

		out := oplogEntry{
			Operation: entry.Operation,
			Timestamp: entry.Timestamp,
			Namespace: entry.Namespace,
			Data:      data,

			TxIdx: *txIdx,
		}

		*txIdx++

		out.Database, out.Collection = parseNamespace(out.Namespace)

		if out.Operation == operationUpdate {
			out.DocID = entry.Update.ID
		} else {
			out.DocID = data["_id"]
		}

		return []oplogEntry{out}

	case operationCommand:
		if entry.Namespace != "admin.$cmd" {
			return nil
		}

		var txData struct {
			ApplyOps []rawOplogEntry `bson:"applyOps"`
		}

		if err := bson.Unmarshal(entry.Doc, &txData); err != nil {
			log.Log.Errorf("unmarshaling transaction data: %v", err)
			return nil
		}

		var ret []oplogEntry

		for _, v := range txData.ApplyOps {
			v.Timestamp = entry.Timestamp
			ret = append(ret, tailer.parseRawOplogEntry(v, txIdx)...)
		}

		return ret

	default:
		return nil
	}
}

// Parses op.Namespace into (database, collection)
func parseNamespace(namespace string) (string, string) {
	namespaceParts := strings.SplitN(namespace, ".", 2)

	database := namespaceParts[0]
	collection := ""
	if len(namespaceParts) > 1 {
		collection = namespaceParts[1]
	}

	return database, collection
}
