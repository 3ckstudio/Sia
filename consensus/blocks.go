package consensus

import (
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/hash"
)

// A non-consensus rule that dictates how much heavier a competing chain has to
// be before the node will switch to mining on that chain. It is set to 5%,
// which actually means that the heavier chain needs to be heavier by 5% of
// _one block_, not 5% heavier as a whole.
//
// This rule is in place because the difficulty gets updated every block, and
// that means that of two competing blocks, one could be very slightly heavier.
// The slightly heavier one should not be switched to if it was not seen first,
// because the amount of extra weight in the chain is inconsequential. The
// maximum difficulty shift will prevent people from manipulating timestamps
// enough to produce a block that is substantially heavier, thus making 5% an
// acceptible value.
var SurpassThreshold = big.NewRat(5, 100)

// Exported Errors
var (
	BlockKnownErr    = errors.New("block exists in block map.")
	FutureBlockErr   = errors.New("timestamp too far in future, will try again later.")
	KnownOrphanErr   = errors.New("block is a known orphan")
	UnknownOrphanErr = errors.New("block is an unknown orphan")
)

// earliestChildTimestamp() returns the earliest timestamp that a child node
// can have while still being valid. See section 'Timestamp Rules' in
// Consensus.md.
//
// TODO: Rather than having the blocknode store the timestamps, blocknodes
// should just point to their parent block, and this function should just crawl
// through the parents.
//
// TODO: After changing how the timestamps are aquired, write some tests to
// check that the timestamp code is working right.
func (bn *BlockNode) earliestChildTimestamp() Timestamp {
	// Get the MedianTimestampWindow previous timestamps and sort them. For
	// now, bn.RecentTimestamps is expected to have the correct timestamps.
	var intTimestamps []int
	for _, timestamp := range bn.RecentTimestamps {
		intTimestamps = append(intTimestamps, int(timestamp))
	}
	sort.Ints(intTimestamps)

	// Return the median of the sorted timestamps.
	return Timestamp(intTimestamps[MedianTimestampWindow/2])
}

// handleOrphanBlock adds a block to the list of orphans, returning an error
// indicating whether the orphan existed previously or not. handleOrphanBlock
// always returns an error.
func (s *State) handleOrphanBlock(b Block) error {
	// Sanity check that the function is being used correctly.
	if DEBUG {
		_, exists := s.blockMap[b.ParentBlockID]
		if exists {
			panic("Incorrect use of handleOrphanBlock")
		}
	}

	// Check if the missing parent is unknown
	missingParent, exists := s.missingParents[b.ParentBlockID]
	if !exists {
		// Add an entry for the parent and add the orphan block to the entry.
		s.missingParents[b.ParentBlockID] = make(map[BlockID]Block)
		s.missingParents[b.ParentBlockID][b.ID()] = b
		return UnknownOrphanErr
	}

	// Check if the orphan is already known, and add the orphan if not.
	_, exists = missingParent[b.ID()]
	if exists {
		return KnownOrphanErr
	}
	missingParent[b.ID()] = b
	return UnknownOrphanErr
}

// checkDestiny determines if the blocks destiny is already known within the
// state, and returns an error if a destiny is discovered. The destiny of a
// block will already be known if the block is an orphan or if the block has
// been seen before.
func (s *State) checkDestiny(b Block) (err error) {
	// See if the block is a known invalid block.
	_, exists := s.badBlocks[b.ID()]
	if exists {
		err = errors.New("block is known to be invalid")
		return
	}

	// See if the block is valid block.
	_, exists = s.blockMap[b.ID()]
	if exists {
		err = BlockKnownErr
		return
	}

	// See if the block is an orphan.
	_, exists = s.blockMap[b.ParentBlockID]
	if !exists {
		err = s.handleOrphanBlock(b)
		return
	}
	return
}

// State.validateHaeader() returns err = nil if the header information in the
// block (everything except the transactions) is valid, and returns an error
// explaining why validation failed if the header is invalid.
func (s *State) validateHeader(parent *BlockNode, b *Block) (err error) {
	// Check the id meets the target.
	if !b.CheckTarget(parent.Target) {
		err = errors.New("block does not meet target")
		return
	}

	// Check that the block is not too far in the future.
	// TODO: sleep for 30 seconds at a time
	skew := b.Timestamp - Timestamp(time.Now().Unix())
	if skew > FutureThreshold {
		go func(skew Timestamp, b Block) {
			time.Sleep(time.Duration(skew-FutureThreshold) * time.Second)
			// s.Lock()
			s.AcceptBlock(b)
			// s.Unlock()
		}(skew, *b)
		err = FutureBlockErr
		return
	}

	// If timestamp is too far in the past, reject and put in bad blocks.
	if parent.earliestChildTimestamp() > b.Timestamp {
		s.badBlocks[b.ID()] = struct{}{}
		err = errors.New("timestamp invalid for being in the past")
		return
	}

	// Check that the transaction merkle root matches the transactions
	// included into the block.
	if b.MerkleRoot != b.TransactionMerkleRoot() {
		s.badBlocks[b.ID()] = struct{}{}
		err = errors.New("merkle root does not match transactions sent.")
		return
	}

	return
}

// State.childTarget() calculates the proper target of a child node given the
// parent node, and copies the target into the child node.
func (s *State) childTarget(parentNode *BlockNode, newNode *BlockNode) Target {
	var timePassed, expectedTimePassed Timestamp
	if newNode.Height < TargetWindow {
		timePassed = newNode.Block.Timestamp - s.blockRoot.Block.Timestamp
		expectedTimePassed = BlockFrequency * Timestamp(newNode.Height)
	} else {
		// THIS CODE ASSUMES THAT THE BLOCK AT HEIGHT
		// NEWNODE.HEIGHT-TARGETWINDOW IS THE SAME FOR BOTH THE NEW NODE AND
		// THE CURRENT FORK. IN GENERAL THIS IS A PRETTY SAFE ASSUMPTION AS ITS
		// LOOKING BACKWARDS BY 5000 BLOCKS. BUT WE SHOULD PROBABLY IMPLEMENT
		// SOMETHING THATS FULLY SAFE REGARDLESS.
		adjustmentBlock, err := s.BlockAtHeight(newNode.Height - TargetWindow)
		if err != nil {
			panic(err)
		}
		timePassed = newNode.Block.Timestamp - adjustmentBlock.Timestamp
		expectedTimePassed = BlockFrequency * Timestamp(TargetWindow)
	}

	// Adjustment = timePassed / expectedTimePassed.
	targetAdjustment := big.NewRat(int64(timePassed), int64(expectedTimePassed))

	// Enforce a maximum targetAdjustment
	if targetAdjustment.Cmp(MaxAdjustmentUp) == 1 {
		targetAdjustment = MaxAdjustmentUp
	} else if targetAdjustment.Cmp(MaxAdjustmentDown) == -1 {
		targetAdjustment = MaxAdjustmentDown
	}

	newTarget := new(big.Rat).Mul(parentNode.Target.Rat(), targetAdjustment)
	return RatToTarget(newTarget)
}

// State.childDepth() returns the cumulative weight of all the blocks leading
// up to and including the child block.
// childDepth := (1/parentTarget + 1/parentDepth)^-1
func (s *State) childDepth(parentNode *BlockNode) (depth Target) {
	cumulativeDifficulty := new(big.Rat).Add(parentNode.Target.Inverse(), parentNode.Depth.Inverse())
	return RatToTarget(new(big.Rat).Inv(cumulativeDifficulty))
}

// State.addBlockToTree() takes a block and a parent node, and adds a child
// node to the parent containing the block. No validation is done.
func (s *State) addBlockToTree(parentNode *BlockNode, b *Block) (newNode *BlockNode) {
	// Create the child node.
	newNode = new(BlockNode)
	newNode.Block = b
	newNode.Height = parentNode.Height + 1

	// Copy over the timestamps.
	copy(newNode.RecentTimestamps[:], parentNode.RecentTimestamps[1:])
	newNode.RecentTimestamps[10] = b.Timestamp

	// Calculate target and depth.
	newNode.Target = s.childTarget(parentNode, newNode)
	newNode.Depth = s.childDepth(parentNode)

	// Add the node to the block map and the list of its parents children.
	s.blockMap[b.ID()] = newNode
	parentNode.Children = append(parentNode.Children, newNode)

	return
}

// State.heavierFork() returns true if the input node is 5% heavier than the
// current node of the ConsensusState.
func (s *State) heavierFork(newNode *BlockNode) bool {
	threshold := new(big.Rat).Mul(s.CurrentBlockWeight(), SurpassThreshold)
	currentCumDiff := s.Depth().Inverse()
	requiredCumDiff := new(big.Rat).Add(currentCumDiff, threshold)
	newNodeCumDiff := newNode.Depth.Inverse()
	return newNodeCumDiff.Cmp(requiredCumDiff) == 1
}

// State.rewindABlock() removes the most recent block from the ConsensusState,
// making the ConsensusState as though the block had never been integrated.
func (s *State) invertRecentBlock() (diffs []OutputDiff) {
	// Remove the output for the miner subsidy.
	//
	// TODO: Update this for incentive stuff - miner doesn't get subsidy until
	// 2000 or 5000 or 10000 blocks later.
	subsidyID := s.CurrentBlock().SubsidyID()
	subsidy, err := s.Output(subsidyID)
	if err != nil {
		panic(err)
	}
	diff := OutputDiff{New: false, ID: subsidyID, Output: subsidy}
	diffs = append(diffs, diff)
	delete(s.unspentOutputs, subsidyID)

	// Perform inverse contract maintenance.
	diffSet := s.invertContractMaintenance()
	diffs = append(diffs, diffSet...)

	// Reverse each transaction in the block, in reverse order from how
	// they appear in the block.
	for i := len(s.CurrentBlock().Transactions) - 1; i >= 0; i-- {
		diffSet := s.invertTransaction(s.CurrentBlock().Transactions[i])
		diffs = append(diffs, diffSet...)
	}

	// Update the CurrentBlock and CurrentPath variables of the longest fork.
	delete(s.currentPath, s.Height())
	s.currentBlockID = s.CurrentBlock().ParentBlockID
	return
}

// s.integrateBlock() will verify the block and then integrate it into the
// consensus state.
func (s *State) integrateBlock(b Block, bd *BlockDiff) (diffs []OutputDiff, err error) {
	bd.CatalystBlock = b.ID()

	var appliedTransactions []Transaction
	minerSubsidy := Currency(0)
	for _, txn := range b.Transactions {
		err = s.ValidTransaction(txn)
		if err != nil {
			break
		}

		// Apply the transaction to the ConsensusState, adding it to the list of applied transactions.
		transactionDiff := s.applyTransaction(txn)
		appliedTransactions = append(appliedTransactions, txn)
		diffs = append(diffs, transactionDiff.OutputDiffs...)
		bd.TransactionDiffs = append(bd.TransactionDiffs, transactionDiff)

		// Add the miner fees to the miner subsidy.
		for _, fee := range txn.MinerFees {
			minerSubsidy += fee
		}
	}

	if err != nil {
		// Rewind transactions added.
		for i := len(appliedTransactions) - 1; i >= 0; i-- {
			s.invertTransaction(appliedTransactions[i])
		}
		return
	}

	// Perform maintanence on all open contracts.
	diffSet := s.applyContractMaintenance(&bd.BlockChanges)
	diffs = append(diffs, diffSet...)

	// Update the current block and current path variables of the longest fork.
	height := s.blockMap[b.ID()].Height
	s.currentBlockID = b.ID()
	s.currentPath[height] = b.ID()

	// Add coin inflation to the miner subsidy.
	minerSubsidy += CalculateCoinbase(s.Height())

	// Add output contianing miner fees + block subsidy.
	//
	// TODO: Add this to the list of future miner subsidies
	minerSubsidyOutput := Output{
		Value:     minerSubsidy,
		SpendHash: b.MinerAddress,
	}
	s.unspentOutputs[b.SubsidyID()] = minerSubsidyOutput
	diff := OutputDiff{New: true, ID: b.SubsidyID(), Output: minerSubsidyOutput}
	diffs = append(diffs, diff)
	bd.BlockChanges.OutputDiffs = append(bd.BlockChanges.OutputDiffs, diffs...)

	return
}

// invalidateNode() is a recursive function that deletes all of the
// children of a block and puts them on the bad blocks list.
func (s *State) invalidateNode(node *BlockNode) {
	for i := range node.Children {
		s.invalidateNode(node.Children[i])
	}

	delete(s.blockMap, node.Block.ID())
	s.badBlocks[node.Block.ID()] = struct{}{}
}

// forkBlockchain() will go from the current block over to a block on a
// different fork, rewinding and integrating blocks as needed. forkBlockchain()
// will return an error if any of the blocks in the new fork are invalid.
func (s *State) forkBlockchain(newNode *BlockNode) (rewoundBlocks []Block, appliedBlocks []Block, outputDiffs []OutputDiff, err error) {
	// Create a block diff for use when calling integrateBlock.
	var cc ConsensusChange

	// Find the common parent between the new fork and the current
	// fork, keeping track of which path is taken through the
	// children of the parents so that we can re-trace as we
	// validate the blocks.
	currentNode := newNode
	value := s.currentPath[currentNode.Height]
	var parentHistory []BlockID
	for value != currentNode.Block.ID() {
		parentHistory = append(parentHistory, currentNode.Block.ID())
		currentNode = s.blockMap[currentNode.Block.ParentBlockID]
		value = s.currentPath[currentNode.Height]
	}

	// Get the state hash before attempting a fork.
	var stateHash hash.Hash
	if DEBUG {
		stateHash = s.StateHash()
	}

	// Remove blocks from the ConsensusState until we get to the
	// same parent that we are forking from.
	for s.currentBlockID != currentNode.Block.ID() {
		rewoundBlocks = append(rewoundBlocks, s.CurrentBlock())
		cc.InvertedBlocks = append(cc.InvertedBlocks, s.currentBlockNode().BlockDiff)
		outputDiffs = append(outputDiffs, s.invertRecentBlock()...)
	}

	// Validate each block in the parent history in order, updating
	// the state as we go.  If at some point a block doesn't
	// verify, you get to walk all the way backwards and forwards
	// again.
	validatedBlocks := 0
	for i := len(parentHistory) - 1; i >= 0; i-- {
		appliedBlock := *s.blockMap[parentHistory[i]].Block
		appliedBlocks = append(appliedBlocks, appliedBlock)
		var bd BlockDiff
		diffSet, err := s.integrateBlock(appliedBlock, &bd)
		if err != nil {
			// Add the whole tree of blocks to BadBlocks,
			// deleting them from BlockMap
			s.invalidateNode(s.blockMap[parentHistory[i]])

			// Rewind the validated blocks
			for i := 0; i < validatedBlocks; i++ {
				s.invertRecentBlock()
			}

			// Integrate the rewound blocks
			for i := len(rewoundBlocks) - 1; i >= 0; i-- {
				_, err = s.integrateBlock(rewoundBlocks[i], &BlockDiff{}) // this diff is not used, because the state has not changed. TODO: change how reapply works.
				if err != nil {
					panic("Once-validated blocks are no longer validating - state logic has mistakes.")
				}
			}

			// Reset diffs to nil since nothing in sum was changed.
			appliedBlocks = nil
			rewoundBlocks = nil
			outputDiffs = nil
			bd = BlockDiff{}

			// Check that the state hash is the same as before forking and then returning.
			if DEBUG {
				if stateHash != s.StateHash() {
					panic("state hash does not match after an unsuccessful fork attempt")
				}
			}

			break
		}
		cc.AppliedBlocks = append(cc.AppliedBlocks, bd)
		s.blockMap[parentHistory[i]].BlockDiff = bd
		// TODO: Add the block diff to the block node, for retrieval during inversion.
		validatedBlocks += 1
		outputDiffs = append(outputDiffs, diffSet...)
	}

	// Update the transaction pool to remove any transactions that have
	// invalidated on account of invalidated storage proofs.
	s.cleanTransactionPool()

	// Notify all subscribers of the changes.
	if appliedBlocks != nil {
		s.notifySubscribers(cc)
	}

	return
}

// State.AcceptBlock() will add blocks to the state, forking the blockchain if
// they are on a fork that is heavier than the current fork.
func (s *State) AcceptBlock(b Block) (rewoundBlocks []Block, appliedBlocks []Block, outputDiffs []OutputDiff, err error) {
	// TODO: Before spending a lot of computational resources on verifying a
	// block, we need to check that the block at least represents a reasonable
	// amount of work done, which will help mitigate certain types of DoS
	// attacks.

	// Check the maps in the state to see if the block is already known.
	err = s.checkDestiny(b)
	if err != nil {
		return
	}
	parentNode := s.blockMap[b.ParentBlockID]

	// Check that the header of the block is acceptible.
	err = s.validateHeader(parentNode, &b)
	if err != nil {
		return
	}

	// Check that the block is the correct size.
	encodedBlock := encoding.Marshal(b)
	if len(encodedBlock) > BlockSizeLimit {
		err = errors.New("Block is too large, will not be accepted.")
		return
	}

	newBlockNode := s.addBlockToTree(parentNode, &b)

	// If the new node is 5% heavier than the current node, switch to the new fork.
	if s.heavierFork(newBlockNode) {
		rewoundBlocks, appliedBlocks, outputDiffs, err = s.forkBlockchain(newBlockNode)
		if err != nil {
			return
		}
	}

	// Notify subscribers of the consensus change.
	var cc ConsensusChange
	s.notifySubscribers(cc)

	// Perform a sanity check if debug flag is set.
	if DEBUG {
		s.CurrentPathCheck()
	}

	return
}
