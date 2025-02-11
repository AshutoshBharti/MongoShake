package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"syscall"

	"mongoshake/collector"
	"mongoshake/collector/ckpt"
	"mongoshake/collector/configure"
	"mongoshake/common"
	"mongoshake/executor"
	"mongoshake/modules"
	"mongoshake/oplog"
	"mongoshake/quorum"

	"github.com/gugemichael/nimo4go"
	LOG "github.com/vinllen/log4go"
	"github.com/vinllen/mgo/bson"
)

type Exit struct{ Code int }

func main() {
	var err error
	defer handleExit()
	defer LOG.Close()
	defer utils.Goodbye()

	// argument options
	configuration := flag.String("conf", "", "configure file absolute path")
	verbose := flag.Bool("verbose", false, "show logs on console")
	version := flag.Bool("version", false, "show version")
	flag.Parse()

	if *configuration == "" || *version == true {
		fmt.Println(utils.BRANCH)
		panic(Exit{0})
	}

	var file *os.File
	if file, err = os.Open(*configuration); err != nil {
		crash(fmt.Sprintf("Configure file open failed. %v", err), -1)
	}

	configure := nimo.NewConfigLoader(file)
	configure.SetDateFormat(utils.GolangSecurityTime)
	if err := configure.Load(&conf.Options); err != nil {
		crash(fmt.Sprintf("Configure file %s parse failed. %v", *configuration, err), -2)
	}

	// verify collector options and revise
	if err = sanitizeOptions(); err != nil {
		crash(fmt.Sprintf("Conf.Options check failed: %s", err.Error()), -4)
	}

	if err := utils.InitialLogger(conf.Options.LogDirectory, conf.Options.LogFileName, conf.Options.LogLevel, conf.Options.LogBuffer, *verbose); err != nil {
		crash(fmt.Sprintf("initial log.dir[%v] log.name[%v] failed[%v].", conf.Options.LogDirectory,
			conf.Options.LogFileName, err), -2)
	}

	conf.Options.Version = utils.BRANCH

	nimo.Profiling(int(conf.Options.SystemProfile))
	nimo.RegisterSignalForProfiling(syscall.SIGUSR2)
	nimo.RegisterSignalForPrintStack(syscall.SIGUSR1, func(bytes []byte) {
		LOG.Info(string(bytes))
	})
	utils.Welcome()

	// get exclusive process lock and write pid
	if utils.WritePidById(conf.Options.LogDirectory, conf.Options.CollectorId) {
		startup()
	}
}

func startup() {
	// leader election at the beginning
	selectLeader()

	// initialize http api
	utils.InitHttpApi(conf.Options.HTTPListenPort)
	coordinator := &collector.ReplicationCoordinator{
		Sources: make([]*utils.MongoSource, len(conf.Options.MongoUrls)),
	}

	utils.HttpApi.RegisterAPI("/conf", nimo.HttpGet, func([]byte) interface{} {
		return &conf.Options
	})

	for i, src := range conf.Options.MongoUrls {
		coordinator.Sources[i] = new(utils.MongoSource)
		coordinator.Sources[i].URL = src
		if len(conf.Options.OplogGIDS) != 0 {
			coordinator.Sources[i].Gids = conf.Options.OplogGIDS
		}
	}

	// start mongodb replication
	if err := coordinator.Run(); err != nil {
		// initial or connection established failed
		crash(fmt.Sprintf("Oplog Tailer initialize failed: %v", err), -6)
	}

	// if the sync mode is "document", mongoshake should exit here.
	if conf.Options.SyncMode != collector.SYNCMODE_DOCUMENT {
		if err := utils.HttpApi.Listen(); err != nil {
			LOG.Critical("Coordinator http api listen failed. %v", err)
		}
	}
}

func selectLeader() {
	// first of all. ensure we are the Master
	if conf.Options.MasterQuorum && conf.Options.ContextStorage == ckpt.StorageTypeDB {
		// election become to Master. keep waiting if we are the candidate. election id is must fixed
		quorum.UseElectionObjectId(bson.ObjectIdHex("5204af979955496907000001"))
		go quorum.BecomeMaster(conf.Options.ContextStorageUrl, utils.AppDatabase)

		// wait until become to a real master
		<-quorum.MasterPromotionNotifier
	} else {
		quorum.AlwaysMaster()
	}
}

func sanitizeOptions() error {
	// compatible with old version
	if len(conf.Options.LogFileNameOld) != 0 {
		conf.Options.LogFileName = conf.Options.LogFileNameOld
	}
	if len(conf.Options.LogLevelOld) != 0 {
		conf.Options.LogLevel = conf.Options.LogLevelOld
	}
	if conf.Options.LogBufferOld == true {
		conf.Options.LogBuffer = conf.Options.LogBufferOld
	}
	if len(conf.Options.LogFileName) == 0 {
		return fmt.Errorf("log.name[%v] shouldn't be empty", conf.Options.LogFileName)
	}

	if len(conf.Options.MongoUrls) == 0 {
		return errors.New("mongo_urls were empty")
	}
	if conf.Options.ContextStorageUrl == "" {
		if len(conf.Options.MongoUrls) == 1 {
			conf.Options.ContextStorageUrl = conf.Options.MongoUrls[0]
		} else if len(conf.Options.MongoUrls) > 1 {
			return errors.New("storage server should be configured while using mongo shard servers")
		}
	}
	if len(conf.Options.MongoUrls) > 1 {
		if conf.Options.WorkerNum != len(conf.Options.MongoUrls) {
			//LOG.Warn("replication worker should be equal to count of mongo_urls while multi sources (shard), set worker = %v",
			//	len(conf.Options.MongoUrls))
			conf.Options.WorkerNum = len(conf.Options.MongoUrls)
		}
		if conf.Options.ReplayerDMLOnly == false {
			return errors.New("DDL is not support for sharding, pleasing waiting")
		}
	}
	// avoid the typo of mongo urls
	if utils.HasDuplicated(conf.Options.MongoUrls) {
		return errors.New("mongo urls were duplicated")
	}
	if conf.Options.CollectorId == "" {
		return errors.New("collector id should not be empty")
	}
	if conf.Options.HTTPListenPort <= 1024 && conf.Options.HTTPListenPort > 0 {
		return errors.New("http listen port too low numeric")
	}
	if conf.Options.CheckpointInterval <= 0 {
		return errors.New("checkpoint batch size is negative")
	}
	if conf.Options.ShardKey != oplog.ShardByNamespace &&
		conf.Options.ShardKey != oplog.ShardByID &&
		conf.Options.ShardKey != oplog.ShardAutomatic {
		return errors.New("shard key type is unknown")
	}
	if conf.Options.SyncerReaderBufferTime == 0 {
		conf.Options.SyncerReaderBufferTime = 1
	}
	if conf.Options.WorkerNum <= 0 || conf.Options.WorkerNum > 256 {
		return errors.New("worker numeric is not valid")
	}
	if conf.Options.WorkerBatchQueueSize <= 0 {
		return errors.New("worker queue numeric is negative")
	}
	if conf.Options.ContextStorage == "" || conf.Options.ContextAddress == "" ||
		(conf.Options.ContextStorage != ckpt.StorageTypeAPI &&
			conf.Options.ContextStorage != ckpt.StorageTypeDB) {
		return errors.New("context storage type or address is invalid")
	}
	if conf.Options.WorkerOplogCompressor != module.CompressionNone &&
		conf.Options.WorkerOplogCompressor != module.CompressionGzip &&
		conf.Options.WorkerOplogCompressor != module.CompressionZlib &&
		conf.Options.WorkerOplogCompressor != module.CompressionDeflate {
		return errors.New("compressor is not supported")
	}
	if conf.Options.MasterQuorum && conf.Options.ContextStorage != ckpt.StorageTypeDB {
		return errors.New("context storage should set to 'database' while master election enabled")
	}
	if len(conf.Options.FilterNamespaceBlack) != 0 &&
		len(conf.Options.FilterNamespaceWhite) != 0 {
		return errors.New("at most one of black lists and white lists option can be given")
	}
	conf.Options.HTTPListenPort = utils.MayBeRandom(conf.Options.HTTPListenPort)
	conf.Options.SystemProfile = utils.MayBeRandom(conf.Options.SystemProfile)

	if conf.Options.Tunnel == "" {
		return errors.New("tunnel is empty")
	}
	if len(conf.Options.TunnelAddress) == 0 && conf.Options.Tunnel != "mock" {
		return errors.New("tunnel address is illegal")
	}
	if conf.Options.SyncMode == "" {
		conf.Options.SyncMode = "oplog" // default
	}

	// judge the replayer configuration when tunnel type is "direct"
	if conf.Options.Tunnel == "direct" {
		if len(conf.Options.TunnelAddress) > conf.Options.WorkerNum {
			return errors.New("then length of tunnel_address with type 'direct' shouldn't bigger than worker number")
		}
		if conf.Options.ReplayerExecutor < 1 {
			return errors.New("executor number should be large than 1")
		}
		if conf.Options.ReplayerConflictWriteTo != executor.DumpConflictToDB &&
			conf.Options.ReplayerConflictWriteTo != executor.DumpConflictToSDK &&
			conf.Options.ReplayerConflictWriteTo != executor.NoDumpConflict {
			return errors.New("collision write strategy is neither db nor sdk nor none")
		}
		conf.Options.ReplayerCollisionEnable = conf.Options.ReplayerExecutor != 1
	} else {
		if conf.Options.SyncMode != "oplog" {
			return errors.New("document replication only support direct tunnel type")
		}
	}

	if conf.Options.SyncMode != "oplog" && conf.Options.SyncMode != "document" && conf.Options.SyncMode != "all" {
		return fmt.Errorf("unknown sync_mode[%v]", conf.Options.SyncMode)
	}

	if conf.Options.MongoConnectMode != utils.ConnectModePrimary &&
		conf.Options.MongoConnectMode != utils.ConnectModeSecondaryPreferred &&
		conf.Options.MongoConnectMode != utils.ConnectModeStandalone {
		return fmt.Errorf("unknown mongo_connect_mode[%v]", conf.Options.MongoConnectMode)
	}

	return nil
}

func crash(msg string, errCode int) {
	fmt.Println(msg)
	panic(Exit{errCode})
}

func handleExit() {
	if e := recover(); e != nil {
		if exit, ok := e.(Exit); ok == true {
			os.Exit(exit.Code)
		}
		panic(e)
	}
}
