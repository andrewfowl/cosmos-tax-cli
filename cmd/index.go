package cmd

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DefiantLabs/lens/client"
	"github.com/go-co-op/gocron"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/cosmos-indexer/core"
	eventTypes "github.com/DefiantLabs/cosmos-indexer/cosmos/events"
	dbTypes "github.com/DefiantLabs/cosmos-indexer/db"
	"github.com/DefiantLabs/cosmos-indexer/osmosis"
	"github.com/DefiantLabs/cosmos-indexer/rpc"
	"github.com/DefiantLabs/cosmos-indexer/tasks"
	"github.com/spf13/cobra"

	"gorm.io/gorm"
)

var reindexMsgType string

func init() {
	indexCmd.Flags().StringVar(&reindexMsgType, "re-index-message-type", "", "If specified, the indexer will reindex only the blocks containing the message type provided.")

	rootCmd.AddCommand(indexCmd)
}

var indexCmd = &cobra.Command{
	Use:   "index",
	Short: "Indexes the blockchain according to the configuration defined.",
	Long: `Indexes the Cosmos-based blockchain according to the configurations found on the command line
	or in the specified config file. Indexes taxable events into a database for easy querying. It is
	highly recommended to keep this command running as a background service to keep your index up to date.`,
	Run: index,
}

// The Indexer struct is used to perform index operations
type Indexer struct {
	cfg       *config.Config
	dryRun    bool
	db        *gorm.DB
	cl        *client.ChainClient
	scheduler *gocron.Scheduler
}

func setupIndexer() *Indexer {
	var idxr Indexer
	var err error
	idxr.cfg, idxr.dryRun, idxr.db, idxr.scheduler, err = setup(conf)
	if err != nil {
		log.Fatalf("Error during application setup. Err: %v", err)
	}

	// Setup chain specific stuff
	core.SetupAddressRegex(idxr.cfg.Lens.AccountPrefix + "(valoper)?1[a-z0-9]{38}")
	core.SetupAddressPrefix(idxr.cfg.Lens.AccountPrefix)
	core.ChainSpecificMessageTypeHandlerBootstrap(idxr.cfg.Lens.ChainID)
	core.ChainSpecificBeginBlockerEventTypeHandlerBootstrap(idxr.cfg.Lens.ChainID)
	core.ChainSpecificEndBlockerEventTypeHandlerBootstrap(idxr.cfg.Lens.ChainID)
	core.ChainSpecificEpochIdentifierEventTypeHandlersBootstrap(idxr.cfg.Lens.ChainID)

	config.SetChainConfig(idxr.cfg.Lens.AccountPrefix)

	// Setup scheduler to periodically update denoms
	if idxr.cfg.Base.API != "" {
		_, err = idxr.scheduler.Every(6).Hours().Do(tasks.IBCDenomUpsertTask, idxr.cfg.Base.API, idxr.db)
		if err != nil {
			config.Log.Error("Error scheduling ibc denom upsert task. Err: ", err)
		}

		idxr.scheduler.StartAsync()
	}

	// Some chains do not have the denom metadata URL available on chain, so we do chain specific downloads instead.
	tasks.DoChainSpecificUpsertDenoms(idxr.db, idxr.cfg.Lens.ChainID)
	idxr.cl = config.GetLensClient(idxr.cfg.Lens)

	// Depending on the app configuration, wait for the chain to catch up
	chainCatchingUp, err := rpc.IsCatchingUp(idxr.cl)
	for idxr.cfg.Base.WaitForChain && chainCatchingUp && err == nil {
		// Wait between status checks, don't spam the node with requests
		config.Log.Debug("Chain is still catching up, please wait or disable check in config.")
		time.Sleep(time.Second * time.Duration(idxr.cfg.Base.WaitForChainDelay))
		chainCatchingUp, err = rpc.IsCatchingUp(idxr.cl)

		// This EOF error pops up from time to time and is unpredictable
		// It is most likely an error on the node, we would need to see any error logs on the node side
		// Try one more time
		if err != nil && strings.HasSuffix(err.Error(), "EOF") {
			time.Sleep(time.Second * time.Duration(idxr.cfg.Base.WaitForChainDelay))
			chainCatchingUp, err = rpc.IsCatchingUp(idxr.cl)
		}
	}
	if err != nil {
		config.Log.Fatal("Error querying chain status.", err)
	}

	if idxr.cfg.Lens.ChainID == osmosis.ChainID && idxr.cfg.Base.EpochEventIndexingEnabled {
		err := osmosis.SetupOsmosisEpochIndexer(idxr.cl, idxr.cfg.Base.EpochIndexingIdentifier)
		if err != nil {
			config.Log.Fatal("Error setting up Osmosis Epoch Indexer.", err)
		}
	}

	return &idxr
}

func index(cmd *cobra.Command, args []string) {
	// Setup the indexer with config, db, and cl
	idxr := setupIndexer()
	dbConn, err := idxr.db.DB()
	if err != nil {
		config.Log.Fatal("Failed to connect to DB", err)
	}
	defer dbConn.Close()

	// blockChan are just the block heights; limit max jobs in the queue, otherwise this queue would contain one
	// item (block height) for every block on the entire blockchain we're indexing. Furthermore, once the queue
	// is close to empty, we will spin up a new thread to fill it up with new jobs.
	blockChan := make(chan int64, 10000)

	// This channel represents query job results for the RPC queries to Cosmos Nodes. Every time an RPC query
	// completes, the query result will be sent to this channel (for later processing by a different thread).
	// Realistically, I expect that RPC queries will be slower than our relational DB on the local network.
	// If RPC queries are faster than DB inserts this buffer will fill up.
	// We will periodically check the buffer size to monitor performance so we can optimize later.
	rpcQueryThreads := int(idxr.cfg.Base.RPCWorkers)
	if rpcQueryThreads == 0 {
		rpcQueryThreads = 4
	} else if rpcQueryThreads > 64 {
		rpcQueryThreads = 64
	}
	txDataChan := make(chan *dbData, 4*rpcQueryThreads)
	var txChanWaitGroup sync.WaitGroup // This group is to ensure we are done getting transactions before we close the TX channel
	// Spin up a (configurable) number of threads to query RPC endpoints for Transactions.
	// this is assumed to be the slowest process that allows concurrency and thus has the most dedicated go routines.
	if idxr.cfg.Base.ChainIndexingEnabled {
		for i := 0; i < rpcQueryThreads; i++ {
			txChanWaitGroup.Add(1)
			go func() {
				idxr.queryRPC(blockChan, txDataChan, core.HandleFailedBlock)
				txChanWaitGroup.Done()
			}()
		}
	}

	// close the transaction chan once all transactions have been written to it
	go func() {
		txChanWaitGroup.Wait()
		close(txDataChan)
	}()

	var wg sync.WaitGroup // This group is to ensure we are done processing transactions and events before returning

	// Block BeginBlocker and EndBlocker indexing requirements. Indexes block events that took place in the BeginBlock and EndBlock state transitions
	blockEventsDataChan := make(chan *blockEventsDBData, 4*rpcQueryThreads)
	if idxr.cfg.Base.BlockEventIndexingEnabled {
		wg.Add(1)
		go idxr.indexBlockEvents(&wg, core.HandleFailedBlock, blockEventsDataChan)
	} else {
		close(blockEventsDataChan)
	}

	chain := dbTypes.Chain{
		ChainID: idxr.cfg.Lens.ChainID,
		Name:    idxr.cfg.Lens.ChainName,
	}
	dbChainID, err := dbTypes.GetDBChainID(idxr.db, chain)
	if err != nil {
		config.Log.Fatal("Failed to add/create chain in DB", err)
	}

	// Epoch BeginBlocker and EndBlocker indexing requirements. Indexes block events that took place in the BeginBlock and EndBlock state transitions of Epochs
	epochEventsDataChan := make(chan *epochEventsDBData, 4*rpcQueryThreads)
	if idxr.cfg.Base.EpochEventIndexingEnabled {
		wg.Add(1)
		go idxr.indexEpochEvents(&wg, core.HandleFailedBlock, epochEventsDataChan, dbChainID)
	} else {
		close(epochEventsDataChan)
	}

	// Start a thread to index the data queried from the chain.
	if idxr.cfg.Base.ChainIndexingEnabled || idxr.cfg.Base.BlockEventIndexingEnabled || idxr.cfg.Base.EpochEventIndexingEnabled {
		wg.Add(1)
		go idxr.doDBUpdates(&wg, txDataChan, blockEventsDataChan, epochEventsDataChan, dbChainID)
	}

	// Add jobs to the queue to be processed
	if idxr.cfg.Base.ChainIndexingEnabled {
		if reindexMsgType != "" {
			idxr.enqueueBlocksToProcessByMsgType(blockChan, dbChainID, reindexMsgType)
		} else {
			idxr.enqueueBlocksToProcess(blockChan, dbChainID)
		}
		// close the block chan once all blocks have been written to it
		close(blockChan)
	}

	// If we error out in the main loop, this will block. Meaning we may not know of an error for 6 hours until last scheduled task stops
	idxr.scheduler.Stop()
	wg.Wait()
}

// enqueueBlocksToProcessByMsgType will pass the blocks containing the specified msg type to the indexer
func (idxr *Indexer) enqueueBlocksToProcessByMsgType(blockChan chan int64, chainID uint, msgType string) {
	// get the block range
	startBlock := idxr.cfg.Base.StartBlock
	endBlock := idxr.cfg.Base.EndBlock
	if endBlock == -1 {
		heighestBlock := dbTypes.GetHighestIndexedBlock(idxr.db, chainID)
		endBlock = heighestBlock.Height
	}

	rows, err := idxr.db.Raw(`SELECT height FROM blocks
							JOIN txes ON txes.block_id = blocks.id
							JOIN messages ON messages.tx_id = txes.id
							JOIN message_types ON message_types.id = messages.message_type_id
							AND message_types.message_type = ?
							WHERE height > ? AND height < ? AND blockchain_id = ?::int;
							`, msgType, startBlock, endBlock, chainID).Rows()
	if err != nil {
		config.Log.Fatalf("Error checking DB for blocks to reindex. Err: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var block int64
		err = idxr.db.ScanRows(rows, &block)
		if err != nil {
			config.Log.Fatal("Error getting block height. Err: %v", err)
		}
		config.Log.Debugf("Sending block %v to be re-indexed.", block)

		if idxr.cfg.Base.Throttling != 0 {
			time.Sleep(time.Second * time.Duration(idxr.cfg.Base.Throttling))
		}

		// Add the new block to the queue
		blockChan <- block
	}
}

func (idxr *Indexer) enqueueFailedBlocks(blockChan chan int64, chainID uint) {
	// Get all failed blocks
	failedBlocks := dbTypes.GetFailedBlocks(idxr.db, chainID)
	if len(failedBlocks) == 0 {
		return
	}
	for _, block := range failedBlocks {
		if idxr.cfg.Base.Throttling != 0 {
			time.Sleep(time.Second * time.Duration(idxr.cfg.Base.Throttling))
		}
		config.Log.Infof("Will re-attempt failed block: %v", block.Height)
		blockChan <- block.Height
	}
	config.Log.Info("All failed blocks have been re-enqueued for processing")
}

// enqueueBlocksToProcess will pass the blocks that need to be processed to the blockchannel
func (idxr *Indexer) enqueueBlocksToProcess(blockChan chan int64, chainID uint) {
	// Unless explicitly prevented, lets attempt to enqueue any failed blocks
	if !idxr.cfg.Base.PreventReattempts {
		idxr.enqueueFailedBlocks(blockChan, chainID)
	}

	// Start at the last indexed block height (or the block height in the config, if set)
	currBlock := idxr.GetIndexerStartingHeight(chainID)
	// Don't index past this block no matter what
	lastBlock := idxr.cfg.Base.EndBlock
	var latestBlock int64 = math.MaxInt64

	// Add jobs to the queue to be processed
	for {
		// The program is configured to stop running after a set block height.
		// Generally this will only be done while debugging or if a particular block was incorrectly processed.
		if lastBlock != -1 && currBlock > lastBlock {
			config.Log.Info("Hit the last block we're allowed to index, exiting enqueue func.")
			return
		} else if idxr.cfg.Base.ExitWhenCaughtUp && currBlock > latestBlock {
			config.Log.Info("Hit the last block we're allowed to index, exiting enqueue func.")
			return
		}

		// The job queue is running out of jobs to process, see if the blockchain has produced any new blocks we haven't indexed yet.
		if len(blockChan) <= cap(blockChan)/4 {
			// This is the latest block height available on the Node.
			var err error
			latestBlock, err = rpc.GetLatestBlockHeight(idxr.cl)
			if err != nil {
				config.Log.Fatal("Error getting blockchain latest height. Err: %v", err)
			}

			// Throttling in case of hitting public APIs
			if idxr.cfg.Base.Throttling != 0 {
				time.Sleep(time.Second * time.Duration(idxr.cfg.Base.Throttling))
			}

			// Already at the latest block, wait for the next block to be available.
			for currBlock <= latestBlock && (currBlock <= lastBlock || lastBlock == -1) && len(blockChan) != cap(blockChan) {
				// if we are not re-indexing, skip curr block if already indexed
				if !idxr.cfg.Base.ReIndex && blockAlreadyIndexed(currBlock, chainID, idxr.db) {
					currBlock++
					continue
				}

				if idxr.cfg.Base.Throttling != 0 {
					time.Sleep(time.Second * time.Duration(idxr.cfg.Base.Throttling))
				}

				// Add the new block to the queue
				blockChan <- currBlock
				currBlock++
			}
		}
	}
}

func GetBlockEventsStartIndexHeight(db *gorm.DB, chainID string) int64 {
	block, err := dbTypes.GetHighestTaxableEventBlock(db, chainID)
	if err != nil && err.Error() != "record not found" {
		log.Fatalf("Cannot retrieve highest indexed block event. Err: %v", err)
	}

	return block.Height
}

// blockAlreadyIndexed will return true if the block is already in the DB
func blockAlreadyIndexed(blockHeight int64, chainID uint, db *gorm.DB) bool {
	var exists bool
	err := db.Raw(`SELECT count(*) > 0 FROM blocks WHERE height = ?::int AND blockchain_id = ?::int AND indexed = true AND time_stamp != '0001-01-01T00:00:00.000Z';`, blockHeight, chainID).Row().Scan(&exists)
	if err != nil {
		config.Log.Fatalf("Error checking DB for block. Err: %v", err)
	}
	return exists
}

// GetIndexerStartingHeight will determine which block to start at
// if start block is set to -1, it will start at the highest block indexed
// otherwise, it will start at the first missing block between the start and end height
func (idxr *Indexer) GetIndexerStartingHeight(chainID uint) int64 {
	// If the start height is set to -1, resume from the highest block already indexed
	if idxr.cfg.Base.StartBlock == -1 {
		latestBlock, err := rpc.GetLatestBlockHeight(idxr.cl)
		if err != nil {
			log.Fatalf("Error getting blockchain latest height. Err: %v", err)
		}

		fmt.Println("Found latest block", latestBlock)
		highestIndexedBlock := dbTypes.GetHighestIndexedBlock(idxr.db, chainID)
		if highestIndexedBlock.Height < latestBlock {
			return highestIndexedBlock.Height + 1
		}
	}

	// if we are re-indexing, just start at the configured start block
	if idxr.cfg.Base.ReIndex {
		return idxr.cfg.Base.StartBlock
	}

	maxStart := idxr.cfg.Base.EndBlock
	if maxStart == -1 {
		heighestBlock := dbTypes.GetHighestIndexedBlock(idxr.db, chainID)
		maxStart = heighestBlock.Height
	}

	// Otherwise, start at the first block after the configured start block that we have not yet indexed.
	return dbTypes.GetFirstMissingBlockInRange(idxr.db, idxr.cfg.Base.StartBlock, maxStart, chainID)
}

// queryRPC will query the RPC endpoint
// this information will be parsed and converted into the domain objects we use for indexing this data.
// data is then passed to a channel to be consumed and inserted into the DB
func (idxr *Indexer) queryRPC(blockChan chan int64, dbDataChan chan *dbData, failedBlockHandler core.FailedBlockHandler) {
	for blockToProcess := range blockChan {
		// attempt to process the block 5 times and then give up
		err := processBlock(idxr.cl, idxr.db, failedBlockHandler, dbDataChan, blockToProcess)
		if err != nil {
			config.Log.Error(fmt.Sprintf("Failed to process block %v. Will add to failed blocks table", blockToProcess))
			err := dbTypes.UpsertFailedBlock(idxr.db, blockToProcess, idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName)
			if err != nil {
				config.Log.Fatal(fmt.Sprintf("Failed to store that block %v failed. Not safe to continue.", blockToProcess), err)
			}
		}
	}
}

func processBlock(cl *client.ChainClient, dbConn *gorm.DB, failedBlockHandler func(height int64, code core.BlockProcessingFailure, err error), dbDataChan chan *dbData, blockToProcess int64) error {
	// fmt.Printf("Querying RPC transactions for block %d\n", blockToProcess)
	newBlock := dbTypes.Block{Height: blockToProcess}
	var txDBWrappers []dbTypes.TxDBWrapper
	var blockTime *time.Time
	var err error
	errTypeURL := false

	txsEventResp, err := rpc.GetTxsByBlockHeight(cl, newBlock.Height)
	if err != nil {
		if strings.Contains(err.Error(), "unable to resolve type URL") {
			errTypeURL = true
		} else {
			config.Log.Errorf("Error getting transactions by block height (%v). Err: %v. Will reattempt", newBlock.Height, err)
			return err
		}
	}

	// There are two reasons this block would be hit
	// 1) The node might have pruned history resulting in a failed lookup. Recheck to see if the block was supposed to have TX results.
	// 2) The RPC endpoint (node we queried) doesn't recognize the type URL anymore, for an older type (e.g. on an archive node).
	if errTypeURL || len(txsEventResp.Txs) == 0 {
		// The node might have pruned history resulting in a failed lookup. Recheck to see if the block was supposed to have TX results.
		resBlockResults, err := rpc.GetBlockByHeight(cl, newBlock.Height)
		if err != nil || resBlockResults == nil {
			if err != nil && strings.Contains(err.Error(), "is not available, lowest height is") {
				failedBlockHandler(newBlock.Height, core.NodeMissingHistoryForBlock, err)
			} else {
				failedBlockHandler(newBlock.Height, core.BlockQueryError, err)
			}
			return err
		} else if len(resBlockResults.TxsResults) > 0 {
			// The tx.height=X query said there were 0 TXs, but GetBlockByHeight() found some. When this happens
			// it is the same on every RPC node. Thus, we defer to the results from GetBlockByHeight.
			config.Log.Debugf("Falling back to secondary queries for block height %d", newBlock.Height)

			blockResults, err := rpc.GetBlock(cl, newBlock.Height)
			if err != nil {
				config.Log.Errorf("Secondary RPC query failed, %d, %s", newBlock.Height, err)
				return err
			}

			txDBWrappers, blockTime, err = core.ProcessRPCBlockByHeightTXs(dbConn, cl, blockResults, resBlockResults)
			if err != nil {
				config.Log.Errorf("Second query parser failed (ProcessRPCBlockByHeightTXs), %d, %s", newBlock.Height, err.Error())
				return err
			}
		}
	} else {
		txDBWrappers, blockTime, err = core.ProcessRPCTXs(dbConn, txsEventResp)
		if err != nil {
			config.Log.Error("ProcessRpcTxs: unhandled error", err)
			failedBlockHandler(blockToProcess, core.UnprocessableTxError, err)
			return err
		}
	}

	// Get the block time if we don't have TXs
	if blockTime == nil {
		result, err := rpc.GetBlock(cl, newBlock.Height)
		if err != nil {
			config.Log.Errorf("Error getting block info for block %v. Err: %v", newBlock.Height, err)
			return err
		}
		blockTime = &result.Block.Time
	}

	res := &dbData{
		txDBWrappers: txDBWrappers,
		blockTime:    *blockTime,
		blockHeight:  blockToProcess,
	}
	dbDataChan <- res

	return nil
}

type dbData struct {
	txDBWrappers []dbTypes.TxDBWrapper
	blockTime    time.Time
	blockHeight  int64
}

type blockEventsDBData struct {
	blockRelevantEvents []eventTypes.EventRelevantInformation
	blockTime           time.Time
	blockHeight         int64
}

type epochEventsDBData struct {
	blockRelevantEvents []eventTypes.EventRelevantInformation
	blockTime           time.Time
	blockHeight         int64
	epochIdentifier     string
	epochNumber         uint
}

func (idxr *Indexer) indexBlockEvents(wg *sync.WaitGroup, failedBlockHandler core.FailedBlockHandler, blockEventsDataChan chan *blockEventsDBData) {
	defer close(blockEventsDataChan)
	defer wg.Done()

	startHeight := idxr.cfg.Base.BlockEventsStartBlock
	endHeight := idxr.cfg.Base.BlockEventsEndBlock

	if startHeight <= 0 {
		dbLastIndexedBlockEvent := GetBlockEventsStartIndexHeight(idxr.db, idxr.cfg.Lens.ChainID)
		if dbLastIndexedBlockEvent > 0 {
			startHeight = dbLastIndexedBlockEvent + 1
		}
	}

	// 0 isn't a valid starting block
	if startHeight <= 0 {
		startHeight = 1
	}

	lastKnownBlockHeight, errBh := rpc.GetLatestBlockHeight(idxr.cl)
	if errBh != nil {
		config.Log.Fatal("Error getting blockchain latest height in block event indexer.", errBh)
	}

	config.Log.Infof("Indexing block events from block: %v to %v", startHeight, endHeight)

	rpcClient := rpc.URIClient{
		Address: idxr.cl.Config.RPCAddr,
		Client:  &http.Client{},
	}

	currentHeight := startHeight

	for endHeight == -1 || currentHeight <= endHeight {
		bresults, err := rpc.GetBlockResultWithRetry(rpcClient, currentHeight, idxr.cfg.Base.RPCRetryAttempts, idxr.cfg.Base.RPCRetryMaxWait)
		if err != nil {
			config.Log.Error(fmt.Sprintf("Error receiving block result for block %d", currentHeight), err)
			failedBlockHandler(currentHeight, core.FailedBlockEventHandling, err)

			err := dbTypes.UpsertFailedEventBlock(idxr.db, currentHeight, idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName)
			if err != nil {
				config.Log.Fatal("Failed to insert failed block event", err)
			}

			currentHeight++
			if idxr.cfg.Base.Throttling != 0 {
				time.Sleep(time.Second * time.Duration(idxr.cfg.Base.Throttling))
			}
			continue
		}

		blockRelevantEvents, err := core.ProcessRPCBlockEvents(bresults)

		switch {
		case err != nil:
			failedBlockHandler(currentHeight, core.FailedBlockEventHandling, err)
			err := dbTypes.UpsertFailedEventBlock(idxr.db, currentHeight, idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName)
			if err != nil {
				config.Log.Fatal("Failed to insert failed block event", err)
			}
		case len(blockRelevantEvents) != 0:
			result, err := rpc.GetBlock(idxr.cl, bresults.Height)
			if err != nil {
				failedBlockHandler(currentHeight, core.FailedBlockEventHandling, err)

				err := dbTypes.UpsertFailedEventBlock(idxr.db, currentHeight, idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName)
				if err != nil {
					config.Log.Fatal("Failed to insert failed block event", err)
				}
			} else {
				blockEventsDataChan <- &blockEventsDBData{
					blockHeight:         bresults.Height,
					blockTime:           result.Block.Time,
					blockRelevantEvents: blockRelevantEvents,
				}
			}
		default:
			config.Log.Infof("Block %d has no relevant block events", bresults.Height)
		}

		currentHeight++

		// Sleep for a bit to allow new blocks to be written to the chain, this allows us to continue the indexer run indefinitely
		if currentHeight > lastKnownBlockHeight {
			config.Log.Infof("Block %d has passed lastKnownBlockHeight, checking again", currentHeight)
			// For loop catches both of the following
			// whether we are going too fast and need to do multiple sleeps
			// whether the lastKnownHeight was set a long time ago (as in at app start) and we just need to reset the value
			for {
				lastKnownBlockHeight, err = rpc.GetLatestBlockHeight(idxr.cl)
				if err != nil {
					config.Log.Fatal("Error getting blockchain latest height in block event indexer.", errBh)
				}

				if currentHeight > lastKnownBlockHeight {
					config.Log.Infof("Sleeping...")
					time.Sleep(time.Second * 20)
				} else {
					config.Log.Infof("Continuing until block %d", lastKnownBlockHeight)
					time.Sleep(time.Second * time.Duration(idxr.cfg.Base.Throttling))
					break
				}
			}
		} else if idxr.cfg.Base.Throttling != 0 {
			time.Sleep(time.Second * time.Duration(idxr.cfg.Base.Throttling))
		}
	}
}

func (idxr *Indexer) indexEpochEvents(wg *sync.WaitGroup, failedBlockHandler core.FailedBlockHandler, epochEventsDataChan chan *epochEventsDBData, chainID uint) {
	defer close(epochEventsDataChan)
	defer wg.Done()

	startEpochNumber := idxr.cfg.Base.EpochEventsStartEpoch
	endEpochNumber := idxr.cfg.Base.EpochEventsEndEpoch
	epochIdentifier := idxr.cfg.Base.EpochIndexingIdentifier

	// Get epochs for identifier between start and end epoch that have not been indexed
	epochsBetween, err := GetUnindexedEpochsAtIdentifierBetweenStartAndEnd(idxr.db, chainID, epochIdentifier, startEpochNumber, endEpochNumber)
	if err != nil {
		config.Log.Fatalf("Error getting epochs between %d and %d for identifier %s. %s", startEpochNumber, endEpochNumber, epochIdentifier, err)
	}

	if len(epochsBetween) == 0 {
		config.Log.Infof("No unindexed epochs found in database between start %d and end %d for epoch identifier %s", startEpochNumber, endEpochNumber, epochIdentifier)
		return
	}

	config.Log.Infof("Indexing epoch events from epoch: %v to %v", startEpochNumber, endEpochNumber)

	rpcClient := rpc.URIClient{
		Address: idxr.cl.Config.RPCAddr,
		Client:  &http.Client{},
	}

	for _, epoch := range epochsBetween {
		config.Log.Infof("Indexing epoch events for epoch %v at height %d", epoch.EpochNumber, epoch.StartHeight)

		bresults, err := rpc.GetBlockResultWithRetry(rpcClient, int64(epoch.StartHeight), idxr.cfg.Base.RPCRetryAttempts, idxr.cfg.Base.RPCRetryMaxWait)
		if err != nil {
			config.Log.Error(fmt.Sprintf("Error receiving block result for block %d", epoch.StartHeight), err)
			failedBlockHandler(int64(epoch.StartHeight), core.FailedBlockEventHandling, err)

			err := dbTypes.UpsertFailedEventBlock(idxr.db, int64(epoch.StartHeight), idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName)
			if err != nil {
				config.Log.Fatal("Failed to insert failed block event", err)
			}

			if idxr.cfg.Base.Throttling != 0 {
				time.Sleep(time.Second * time.Duration(idxr.cfg.Base.Throttling))
			}
			continue
		}

		blockRelevantEvents, err := core.ProcessRPCEpochEvents(bresults, epochIdentifier)

		if err != nil {
			failedBlockHandler(int64(epoch.StartHeight), core.FailedBlockEventHandling, err)
			err := dbTypes.UpsertFailedEventBlock(idxr.db, int64(epoch.StartHeight), idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName)
			if err != nil {
				config.Log.Fatal("Failed to insert failed block event", err)
			}
		} else {
			if len(blockRelevantEvents) == 0 {
				config.Log.Infof("Block %d has no relevant block events", bresults.Height)
			}

			result, err := rpc.GetBlock(idxr.cl, bresults.Height)
			if err != nil {
				failedBlockHandler(int64(epoch.StartHeight), core.FailedBlockEventHandling, err)

				err := dbTypes.UpsertFailedEventBlock(idxr.db, int64(epoch.StartHeight), idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName)
				if err != nil {
					config.Log.Fatal("Failed to insert failed block event", err)
				}
			} else {
				epochEventsDataChan <- &epochEventsDBData{
					blockHeight:         bresults.Height,
					blockTime:           result.Block.Time,
					blockRelevantEvents: blockRelevantEvents,
					epochIdentifier:     epochIdentifier,
					epochNumber:         epoch.EpochNumber,
				}
			}
		}

		if idxr.cfg.Base.Throttling != 0 {
			time.Sleep(time.Second * time.Duration(idxr.cfg.Base.Throttling))
		}

	}

	config.Log.Infof("Finished gathering epoch events for epochs %d to %d in identifier %s", startEpochNumber, endEpochNumber, epochIdentifier)
}

func GetUnindexedEpochsAtIdentifierBetweenStartAndEnd(db *gorm.DB, chainID uint, identifier string, startEpochNumber int64, endEpochNumber int64) ([]dbTypes.Epoch, error) {
	var epochsBetween []dbTypes.Epoch
	dbResp := db.Where("epoch_number >= ? AND epoch_number <= ? AND identifier=? AND blockchain_id=? AND indexed=False", startEpochNumber, endEpochNumber, identifier, chainID).Find(&epochsBetween)
	return epochsBetween, dbResp.Error
}

// doDBUpdates will read the data out of the db data chan that had been processed by the workers
// if this is a dry run, we will simply empty the channel and track progress
// otherwise we will index the data in the DB.
// it will also read rewars data and index that.
func (idxr *Indexer) doDBUpdates(wg *sync.WaitGroup, txDataChan chan *dbData, blockEventsDataChan chan *blockEventsDBData, epochEventsDataChan chan *epochEventsDBData, dbChainID uint) {
	blocksProcessed := 0
	dbWrites := 0
	dbReattempts := 0
	timeStart := time.Now()
	defer wg.Done()

	for {
		// break out of loop once all channels are fully consumed
		if txDataChan == nil && blockEventsDataChan == nil && epochEventsDataChan == nil {
			config.Log.Info("DB updates complete")
			break
		}

		select {
		// read tx data from the data chan
		case data, ok := <-txDataChan:
			if !ok {
				txDataChan = nil
				continue
			}
			dbWrites++
			// While debugging we'll sometimes want to turn off INSERTS to the DB
			// Note that this does not turn off certain reads or DB connections.
			if !idxr.dryRun {
				config.Log.Info(fmt.Sprintf("Indexing %v TXs from block %d", len(data.txDBWrappers), data.blockHeight))
				err := dbTypes.IndexNewBlock(idxr.db, data.blockHeight, data.blockTime, data.txDBWrappers, dbChainID)
				if err != nil {
					// Do a single reattempt on failure
					dbReattempts++
					err = dbTypes.IndexNewBlock(idxr.db, data.blockHeight, data.blockTime, data.txDBWrappers, dbChainID)
					if err != nil {
						config.Log.Fatal(fmt.Sprintf("Error indexing block %v.", data.blockHeight), err)
					}
				}
			} else {
				config.Log.Info(fmt.Sprintf("Processing block %d (dry run, block data will not be stored in DB).", data.blockHeight))
			}

			// Just measuring how many blocks/second we can process
			if idxr.cfg.Base.BlockTimer > 0 {
				blocksProcessed++
				if blocksProcessed%int(idxr.cfg.Base.BlockTimer) == 0 {
					totalTime := time.Since(timeStart)
					config.Log.Info(fmt.Sprintf("Processing %d blocks took %f seconds. %d total blocks have been processed.\n", idxr.cfg.Base.BlockTimer, totalTime.Seconds(), blocksProcessed))
					timeStart = time.Now()
				}
				if float64(dbReattempts)/float64(dbWrites) > .1 {
					config.Log.Fatalf("More than 10%% of the last %v DB writes have failed.", dbWrites)
				}
			}
		case eventData, ok := <-blockEventsDataChan:
			if !ok {
				blockEventsDataChan = nil
				continue
			}
			dbWrites++
			config.Log.Info(fmt.Sprintf("Indexing %v Block Events from block %d", len(eventData.blockRelevantEvents), eventData.blockHeight))
			identifierLoggingString := fmt.Sprintf("block %d", eventData.blockHeight)

			err := dbTypes.IndexBlockEvents(idxr.db, idxr.dryRun, eventData.blockHeight, eventData.blockTime, eventData.blockRelevantEvents, idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName, identifierLoggingString)
			if err != nil {
				// Do a single reattempt on failure
				dbReattempts++
				err = dbTypes.IndexBlockEvents(idxr.db, idxr.dryRun, eventData.blockHeight, eventData.blockTime, eventData.blockRelevantEvents, idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName, identifierLoggingString)
				if err != nil {
					config.Log.Fatal(fmt.Sprintf("Error indexing block events for %s.", identifierLoggingString), err)
				}
			}
		case epochEventData, ok := <-epochEventsDataChan:

			if !ok {
				epochEventsDataChan = nil
				continue
			}
			dbWrites++
			identifierLoggingString := fmt.Sprintf("epoch %d in epoch identifier %s", epochEventData.epochNumber, epochEventData.epochIdentifier)
			config.Log.Info(fmt.Sprintf("Indexing %v Block Events from block %d for %s", len(epochEventData.blockRelevantEvents), epochEventData.blockHeight, identifierLoggingString))

			err := dbTypes.IndexBlockEvents(idxr.db, idxr.dryRun, epochEventData.blockHeight, epochEventData.blockTime, epochEventData.blockRelevantEvents, idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName, identifierLoggingString)
			if err != nil {
				// Do a single reattempt on failure
				dbReattempts++
				err = dbTypes.IndexBlockEvents(idxr.db, idxr.dryRun, epochEventData.blockHeight, epochEventData.blockTime, epochEventData.blockRelevantEvents, idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName, identifierLoggingString)
				if err != nil {
					config.Log.Fatal(fmt.Sprintf("Error indexing block events for %s.", identifierLoggingString), err)
				}
			}

			err = dbTypes.UpdateEpochIndexingStatus(idxr.db, idxr.dryRun, epochEventData.epochNumber, epochEventData.epochIdentifier, idxr.cfg.Lens.ChainID, idxr.cfg.Lens.ChainName)

			if err != nil {
				config.Log.Fatal(fmt.Sprintf("Error indexing block events for %s. Could not mark Epoch indexed.", identifierLoggingString), err)
			}
		}
	}
}
