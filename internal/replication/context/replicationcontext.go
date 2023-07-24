/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements. See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package context

import (
	"context"
	"github.com/go-errors/errors"
	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	spiconfig "github.com/noctarius/timescaledb-event-streamer/spi/config"
	"github.com/noctarius/timescaledb-event-streamer/spi/pgtypes"
	"github.com/noctarius/timescaledb-event-streamer/spi/statestorage"
	"github.com/noctarius/timescaledb-event-streamer/spi/systemcatalog"
	"github.com/noctarius/timescaledb-event-streamer/spi/version"
	"github.com/samber/lo"
	"github.com/urfave/cli"
	"sync"
)

type ReplicationContextProvider func(
	config *spiconfig.Config, pgxConfig *pgx.ConnConfig,
	stateStorageManager statestorage.Manager,
	sideChannelProvider SideChannelProvider,
) (ReplicationContext, error)

type HypertableSchemaCallback = func(hypertable *systemcatalog.Hypertable, columns []systemcatalog.Column) bool

type SnapshotRowCallback = func(lsn pgtypes.LSN, values map[string]any) error

type ReplicationContext interface {
	StartReplicationContext() error
	StopReplicationContext() error
	NewSideChannelConnection(ctx context.Context) (*pgx.Conn, error)
	NewReplicationChannelConnection(ctx context.Context) (*pgconn.PgConn, error)

	PublicationManager() PublicationManager
	StateStorageManager() statestorage.Manager
	TaskManager() TaskManager
	TypeResolver() pgtypes.TypeResolver

	Offset() (*statestorage.Offset, error)
	SetLastTransactionId(xid uint32)
	LastTransactionId() uint32
	SetLastBeginLSN(lsn pgtypes.LSN)
	LastBeginLSN() pgtypes.LSN
	SetLastCommitLSN(lsn pgtypes.LSN)
	LastCommitLSN() pgtypes.LSN
	AcknowledgeReceived(xld pgtypes.XLogData)
	LastReceivedLSN() pgtypes.LSN
	AcknowledgeProcessed(xld pgtypes.XLogData, processedLSN *pgtypes.LSN) error
	LastProcessedLSN() pgtypes.LSN
	SetPositionLSNs(receivedLSN, processedLSN pgtypes.LSN)

	InitialSnapshotMode() spiconfig.InitialSnapshotMode
	DatabaseUsername() string
	ReplicationSlotName() string
	ReplicationSlotCreate() bool
	ReplicationSlotAutoDrop() bool
	WALLevel() string
	SystemId() string
	Timeline() int32
	DatabaseName() string

	PostgresVersion() version.PostgresVersion
	TimescaleVersion() version.TimescaleVersion
	IsMinimumPostgresVersion() bool
	IsPG14GE() bool
	IsMinimumTimescaleVersion() bool
	IsTSDB212GE() bool
	IsLogicalReplicationEnabled() bool

	HasTablePrivilege(entity systemcatalog.SystemEntity, grant Grant) (access bool, err error)
	LoadHypertables(cb func(hypertable *systemcatalog.Hypertable) error) error
	LoadChunks(cb func(chunk *systemcatalog.Chunk) error) error
	ReadHypertableSchema(
		cb HypertableSchemaCallback,
		pgTypeResolver func(oid uint32) (pgtypes.PgType, error),
		hypertables ...*systemcatalog.Hypertable,
	) error
	SnapshotChunkTable(chunk *systemcatalog.Chunk, cb SnapshotRowCallback) (pgtypes.LSN, error)
	FetchHypertableSnapshotBatch(
		hypertable *systemcatalog.Hypertable, snapshotName string, cb SnapshotRowCallback,
	) error
	ReadSnapshotHighWatermark(hypertable *systemcatalog.Hypertable, snapshotName string) (map[string]any, error)
	ReadReplicaIdentity(entity systemcatalog.SystemEntity) (pgtypes.ReplicaIdentity, error)
	ReadContinuousAggregate(materializedHypertableId int32) (viewSchema, viewName string, found bool, err error)
	ExistsReplicationSlot(slotName string) (found bool, err error)
	ReadReplicationSlot(
		slotName string,
	) (pluginName, slotType string, restartLsn, confirmedFlushLsn pgtypes.LSN, err error)
}

type replicationContext struct {
	pgxConfig *pgx.ConnConfig

	sideChannel SideChannel

	// internal manager classes
	publicationManager  *publicationManager
	stateStorageManager statestorage.Manager
	taskManager         *taskManager

	snapshotInitialMode     spiconfig.InitialSnapshotMode
	snapshotBatchSize       int
	publicationName         string
	publicationCreate       bool
	publicationAutoDrop     bool
	replicationSlotName     string
	replicationSlotCreate   bool
	replicationSlotAutoDrop bool

	timeline          int32
	systemId          string
	databaseName      string
	walLevel          string
	lsnMutex          sync.Mutex
	lastBeginLSN      pgtypes.LSN
	lastCommitLSN     pgtypes.LSN
	lastReceivedLSN   pgtypes.LSN
	lastProcessedLSN  pgtypes.LSN
	lastTransactionId uint32

	pgVersion   version.PostgresVersion
	tsdbVersion version.TimescaleVersion
}

func NewReplicationContext(
	config *spiconfig.Config, pgxConfig *pgx.ConnConfig,
	stateStorageManager statestorage.Manager,
	sideChannelProvider SideChannelProvider,
) (ReplicationContext, error) {

	publicationName := spiconfig.GetOrDefault(
		config, spiconfig.PropertyPostgresqlPublicationName, "",
	)
	publicationCreate := spiconfig.GetOrDefault(
		config, spiconfig.PropertyPostgresqlPublicationCreate, true,
	)
	publicationAutoDrop := spiconfig.GetOrDefault(
		config, spiconfig.PropertyPostgresqlPublicationAutoDrop, true,
	)
	snapshotInitialMode := spiconfig.GetOrDefault(
		config, spiconfig.PropertyPostgresqlSnapshotInitialMode, spiconfig.Never,
	)
	snapshotBatchSize := spiconfig.GetOrDefault(
		config, spiconfig.PropertyPostgresqlSnapshotBatchsize, 1000,
	)
	replicationSlotName := spiconfig.GetOrDefault(
		config, spiconfig.PropertyPostgresqlReplicationSlotName, lo.RandomString(20, lo.LowerCaseLettersCharset),
	)
	replicationSlotCreate := spiconfig.GetOrDefault(
		config, spiconfig.PropertyPostgresqlReplicationSlotCreate, true,
	)
	replicationSlotAutoDrop := spiconfig.GetOrDefault(
		config, spiconfig.PropertyPostgresqlReplicationSlotAutoDrop, true,
	)

	taskManager, err := newTaskManager(config)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	// Build the replication context to be passed along in terms of
	// potential interface implementations to break up internal dependencies
	replicationContext := &replicationContext{
		pgxConfig: pgxConfig,

		taskManager:         taskManager,
		stateStorageManager: stateStorageManager,

		snapshotInitialMode:     snapshotInitialMode,
		snapshotBatchSize:       snapshotBatchSize,
		publicationName:         publicationName,
		publicationCreate:       publicationCreate,
		publicationAutoDrop:     publicationAutoDrop,
		replicationSlotName:     replicationSlotName,
		replicationSlotCreate:   replicationSlotCreate,
		replicationSlotAutoDrop: replicationSlotAutoDrop,
	}

	// Instantiate the actual side channel implementation
	// which handles queries against the database
	sideChannel, err := sideChannelProvider(replicationContext)
	if err != nil {
		return nil, err
	}
	replicationContext.sideChannel = sideChannel

	pgVersion, err := sideChannel.GetPostgresVersion()
	if err != nil {
		return nil, err
	}
	replicationContext.pgVersion = pgVersion

	tsdbVersion, found, err := sideChannel.GetTimescaleDBVersion()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, cli.NewExitError("TimescaleDB extension not found", 17)
	}
	replicationContext.tsdbVersion = tsdbVersion

	databaseName, systemId, timeline, err := sideChannel.GetSystemInformation()
	if err != nil {
		return nil, err
	}
	replicationContext.databaseName = databaseName
	replicationContext.systemId = systemId
	replicationContext.timeline = timeline

	walLevel, err := sideChannel.GetWalLevel()
	if err != nil {
		return nil, err
	}
	replicationContext.walLevel = walLevel

	// Set up internal manager classes
	replicationContext.publicationManager = &publicationManager{
		replicationContext: replicationContext,
	}
	return replicationContext, nil
}

func (rc *replicationContext) PublicationManager() PublicationManager {
	return rc.publicationManager
}

func (rc *replicationContext) StateStorageManager() statestorage.Manager {
	return rc.stateStorageManager
}

func (rc *replicationContext) TaskManager() TaskManager {
	return rc.taskManager
}

func (rc *replicationContext) TypeResolver() pgtypes.TypeResolver {
	return rc.sideChannel
}

func (rc *replicationContext) StartReplicationContext() error {
	rc.taskManager.StartDispatcher()
	return rc.stateStorageManager.Start()
}

func (rc *replicationContext) StopReplicationContext() error {
	if err := rc.taskManager.StopDispatcher(); err != nil {
		return err
	}
	return rc.stateStorageManager.Stop()
}

func (rc *replicationContext) Offset() (*statestorage.Offset, error) {
	offsets, err := rc.stateStorageManager.Get()
	if err != nil {
		return nil, err
	}
	if offsets == nil {
		return nil, nil
	}
	if o, present := offsets[rc.replicationSlotName]; present {
		return o, nil
	}
	return nil, nil
}

func (rc *replicationContext) SetLastTransactionId(xid uint32) {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	rc.lastTransactionId = xid
}

func (rc *replicationContext) LastTransactionId() uint32 {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	return rc.lastTransactionId
}

func (rc *replicationContext) SetLastBeginLSN(lsn pgtypes.LSN) {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	rc.lastBeginLSN = lsn
}

func (rc *replicationContext) LastBeginLSN() pgtypes.LSN {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	return rc.lastBeginLSN
}

func (rc *replicationContext) SetLastCommitLSN(lsn pgtypes.LSN) {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	rc.lastCommitLSN = lsn
}

func (rc *replicationContext) LastCommitLSN() pgtypes.LSN {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	return rc.lastCommitLSN
}

func (rc *replicationContext) LastReceivedLSN() pgtypes.LSN {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	return rc.lastReceivedLSN
}

func (rc *replicationContext) LastProcessedLSN() pgtypes.LSN {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	return rc.lastProcessedLSN
}

func (rc *replicationContext) SetPositionLSNs(receivedLSN, processedLSN pgtypes.LSN) {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	rc.lastReceivedLSN = receivedLSN
	rc.lastProcessedLSN = processedLSN
}

func (rc *replicationContext) AcknowledgeReceived(xld pgtypes.XLogData) {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	rc.lastReceivedLSN = pgtypes.LSN(xld.WALStart + pglogrepl.LSN(len(xld.WALData)))
}

func (rc *replicationContext) AcknowledgeProcessed(xld pgtypes.XLogData, processedLSN *pgtypes.LSN) error {
	rc.lsnMutex.Lock()
	defer rc.lsnMutex.Unlock()

	newLastProcessedLSN := pgtypes.LSN(xld.WALStart + pglogrepl.LSN(len(xld.WALData)))
	if processedLSN != nil {
		rc.taskManager.logger.Debugf("Acknowledge transaction end: %s", processedLSN)
		newLastProcessedLSN = *processedLSN
	}

	if newLastProcessedLSN > rc.lastProcessedLSN {
		rc.lastProcessedLSN = newLastProcessedLSN
	}

	o, err := rc.Offset()
	if err != nil {
		return err
	}

	if o == nil {
		o = &statestorage.Offset{}
	}

	o.LSN = rc.lastProcessedLSN
	o.Timestamp = xld.ServerTime

	return rc.stateStorageManager.Set(rc.replicationSlotName, o)
}

func (rc *replicationContext) InitialSnapshotMode() spiconfig.InitialSnapshotMode {
	return rc.snapshotInitialMode
}

func (rc *replicationContext) DatabaseUsername() string {
	return rc.pgxConfig.User
}

func (rc *replicationContext) ReplicationSlotName() string {
	return rc.replicationSlotName
}

func (rc *replicationContext) ReplicationSlotCreate() bool {
	return rc.replicationSlotCreate
}

func (rc *replicationContext) ReplicationSlotAutoDrop() bool {
	return rc.replicationSlotAutoDrop
}

func (rc *replicationContext) WALLevel() string {
	return rc.walLevel
}

func (rc *replicationContext) SystemId() string {
	return rc.systemId
}

func (rc *replicationContext) Timeline() int32 {
	return rc.timeline
}

func (rc *replicationContext) DatabaseName() string {
	return rc.databaseName
}

func (rc *replicationContext) PostgresVersion() version.PostgresVersion {
	return rc.pgVersion
}

func (rc *replicationContext) TimescaleVersion() version.TimescaleVersion {
	return rc.tsdbVersion
}

func (rc *replicationContext) IsMinimumPostgresVersion() bool {
	return rc.pgVersion >= version.PG_MIN_VERSION
}

func (rc *replicationContext) IsPG14GE() bool {
	return rc.pgVersion >= version.PG_14_VERSION
}

func (rc *replicationContext) IsMinimumTimescaleVersion() bool {
	return rc.tsdbVersion >= version.TSDB_MIN_VERSION
}

func (rc *replicationContext) IsTSDB212GE() bool {
	return rc.tsdbVersion >= version.TSDB_212_VERSION
}

func (rc *replicationContext) IsLogicalReplicationEnabled() bool {
	return rc.walLevel == "logical"
}

// ----> SideChannel functions

func (rc *replicationContext) HasTablePrivilege(
	entity systemcatalog.SystemEntity, grant Grant) (access bool, err error) {

	return rc.sideChannel.HasTablePrivilege(rc.pgxConfig.User, entity, grant)
}

func (rc *replicationContext) LoadHypertables(cb func(hypertable *systemcatalog.Hypertable) error) error {
	return rc.sideChannel.ReadHypertables(cb)
}

func (rc *replicationContext) LoadChunks(cb func(chunk *systemcatalog.Chunk) error) error {
	return rc.sideChannel.ReadChunks(cb)
}

func (rc *replicationContext) ReadHypertableSchema(
	cb func(hypertable *systemcatalog.Hypertable, columns []systemcatalog.Column) bool,
	pgTypeResolver func(oid uint32) (pgtypes.PgType, error),
	hypertables ...*systemcatalog.Hypertable) error {

	return rc.sideChannel.ReadHypertableSchema(cb, pgTypeResolver, hypertables...)
}

func (rc *replicationContext) SnapshotChunkTable(
	chunk *systemcatalog.Chunk, cb SnapshotRowCallback,
) (pgtypes.LSN, error) {

	return rc.sideChannel.SnapshotChunkTable(chunk, rc.snapshotBatchSize, cb)
}

func (rc *replicationContext) FetchHypertableSnapshotBatch(
	hypertable *systemcatalog.Hypertable, snapshotName string, cb SnapshotRowCallback,
) error {

	return rc.sideChannel.FetchHypertableSnapshotBatch(hypertable, snapshotName, rc.snapshotBatchSize, cb)
}

func (rc *replicationContext) ReadSnapshotHighWatermark(
	hypertable *systemcatalog.Hypertable, snapshotName string,
) (map[string]any, error) {

	return rc.sideChannel.ReadSnapshotHighWatermark(hypertable, snapshotName)
}

func (rc *replicationContext) ReadReplicaIdentity(entity systemcatalog.SystemEntity) (pgtypes.ReplicaIdentity, error) {
	return rc.sideChannel.ReadReplicaIdentity(entity.SchemaName(), entity.TableName())
}

func (rc *replicationContext) ReadContinuousAggregate(
	materializedHypertableId int32,
) (viewSchema, viewName string, found bool, err error) {

	return rc.sideChannel.ReadContinuousAggregate(materializedHypertableId)
}

func (rc *replicationContext) ExistsReplicationSlot(slotName string) (found bool, err error) {
	return rc.sideChannel.ExistsReplicationSlot(slotName)
}

func (rc *replicationContext) ReadReplicationSlot(
	slotName string,
) (pluginName, slotType string, restartLsn, confirmedFlushLsn pgtypes.LSN, err error) {

	return rc.sideChannel.ReadReplicationSlot(slotName)
}

func (rc *replicationContext) NewSideChannelConnection(ctx context.Context) (*pgx.Conn, error) {
	return pgx.ConnectConfig(ctx, rc.pgxConfig)
}

func (rc *replicationContext) NewReplicationChannelConnection(ctx context.Context) (*pgconn.PgConn, error) {
	connConfig := rc.pgxConfig.Config.Copy()
	if connConfig.RuntimeParams == nil {
		connConfig.RuntimeParams = make(map[string]string)
	}
	connConfig.RuntimeParams["replication"] = "database"
	return pgconn.ConnectConfig(ctx, connConfig)
}
