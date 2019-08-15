package plugin

import (
	"errors"
	"sync"

	"github.com/PlatONnetwork/PlatON-Go/params"

	"github.com/PlatONnetwork/PlatON-Go/x/staking"

	"github.com/PlatONnetwork/PlatON-Go/common/byteutil"

	"github.com/PlatONnetwork/PlatON-Go/common"
	"github.com/PlatONnetwork/PlatON-Go/core/types"
	"github.com/PlatONnetwork/PlatON-Go/log"
	"github.com/PlatONnetwork/PlatON-Go/p2p/discover"
	"github.com/PlatONnetwork/PlatON-Go/x/gov"
	"github.com/PlatONnetwork/PlatON-Go/x/xcom"
	"github.com/PlatONnetwork/PlatON-Go/x/xutil"
)

var (
	govPluginOnce sync.Once
)

type GovPlugin struct {
	govDB *gov.GovDB
}

var govp *GovPlugin

func GovPluginInstance() *GovPlugin {
	govPluginOnce.Do(func() {
		log.Info("Init Governance plugin ...")
		govp = &GovPlugin{govDB: gov.GovDBInstance()}
	})
	return govp
}

func (govPlugin *GovPlugin) Confirmed(block *types.Block) error {
	return nil
}

//implement BasePlugin
func (govPlugin *GovPlugin) BeginBlock(blockHash common.Hash, header *types.Header, state xcom.StateDB) error {
	var blockNumber = header.Number.Uint64()
	log.Debug("call BeginBlock()", "blockNumber", blockNumber, "blockHash", blockHash)

	//check if there's a pre-active version proposal that can be activated
	preActiveVersionProposalID, err := govPlugin.govDB.GetPreActiveProposalID(blockHash)
	if err != nil {
		log.Error("check if there's a pre-active version proposal failed.", "blockNumber", blockNumber, "blockHash", blockHash)
		return err
	}
	if preActiveVersionProposalID == common.ZeroHash {
		return nil
	}

	//handle a PreActiveProposal
	preActiveVersionProposal, err := govPlugin.govDB.GetExistProposal(preActiveVersionProposalID, state)
	if err != nil {
		return err
	}
	versionProposal, isVersionProposal := preActiveVersionProposal.(gov.VersionProposal)

	if isVersionProposal {
		log.Debug("found pre-active version proposal", "proposalID", preActiveVersionProposalID, "blockNumber", blockNumber, "blockHash", blockHash, "activeBlockNumber", versionProposal.GetActiveBlock())
		if blockNumber >= versionProposal.GetActiveBlock() && (blockNumber-versionProposal.GetActiveBlock())%xutil.ConsensusSize() == 0 {
			currentValidatorList, err := stk.ListCurrentValidatorID(blockHash, blockNumber)
			if err != nil {
				log.Error("list current round validators failed.", "blockHash", blockHash, "blockNumber", blockNumber)
				return err
			}
			var updatedNodes int = 0
			var totalValidators int = len(currentValidatorList)

			//all active validators (including node that has either voted or declared)
			activeList, err := govPlugin.govDB.GetActiveNodeList(blockHash, preActiveVersionProposalID)
			if err != nil {
				log.Error("list all active nodes failed.", "blockNumber", blockNumber, "blockHash", blockHash, "preActiveVersionProposalID", preActiveVersionProposalID)
				return err
			}

			//check if all validators are active
			for _, validator := range currentValidatorList {
				if xutil.InNodeIDList(validator, activeList) {
					updatedNodes++
				}
			}

			log.Debug("check active criteria", "blockNumber", blockNumber, "blockHash", blockHash, "pre-active nodes", updatedNodes, "total validators", totalValidators, "activeList", activeList, "currentValidator", currentValidatorList)
			if updatedNodes == totalValidators {
				log.Debug("the pre-active version proposal has passed")
				tallyResult, err := govPlugin.govDB.GetTallyResult(preActiveVersionProposalID, state)
				if err != nil {
					log.Error("find tally result by proposal ID failed.", "blockNumber", blockNumber, "blockHash", blockHash, "preActiveVersionProposalID", preActiveVersionProposalID)
					return err
				}
				//change tally status to "active"
				tallyResult.Status = gov.Active

				if err := govPlugin.govDB.SetTallyResult(*tallyResult, state); err != nil {
					log.Error("update version proposal tally result failed.", "preActiveVersionProposalID", preActiveVersionProposalID)
					return err
				}

				if versionProposal.GetActiveBlock() != blockNumber {
					versionProposal.ActiveBlock = blockNumber
					if err := govPlugin.govDB.SetProposal(versionProposal, state); err != nil {
						log.Error("update activeBlock of version proposal failed.", "preActiveVersionProposalID", preActiveVersionProposalID, "blockNumber", blockNumber, "blockHash", blockHash)
					}
				}

				if err = govPlugin.govDB.MovePreActiveProposalIDToEnd(blockHash, preActiveVersionProposalID, state); err != nil {
					log.Error("move version proposal ID to EndProposalID list failed.", "blockNumber", blockNumber, "blockHash", blockHash, "preActiveVersionProposalID", preActiveVersionProposalID)
					return err
				}

				if err = govPlugin.govDB.ClearActiveNodes(blockHash, preActiveVersionProposalID); err != nil {
					log.Error("clear version proposal active nodes failed.", "blockNumber", blockNumber, "blockHash", blockHash, "preActiveVersionProposalID", preActiveVersionProposalID)
					return err
				}

				if err = govPlugin.govDB.AddActiveVersion(versionProposal.NewVersion, blockNumber, state); err != nil {
					log.Error("save active version to stateDB failed.", "blockNumber", blockNumber, "blockHash", blockHash, "preActiveProposalID", preActiveVersionProposalID)
					return err
				}
				log.Debug("PlatON is ready to upgrade to new version.")
			}
		}
	}

	return nil
}

//implement BasePlugin
func (govPlugin *GovPlugin) EndBlock(blockHash common.Hash, header *types.Header, state xcom.StateDB) error {
	var blockNumber = header.Number.Uint64()
	log.Debug("call EndBlock()", "blockNumber", blockNumber, "blockHash", blockHash)

	votingProposalIDs, err := govPlugin.govDB.ListVotingProposal(blockHash)
	if err != nil {
		return err
	}
	if len(votingProposalIDs) == 0 {
		log.Debug("there's no voting proposal", "blockNumber", blockNumber, "blockHash", blockHash)
		return nil
	}

	verifierList, err := stk.ListVerifierNodeID(blockHash, blockNumber)
	if err != nil {
		return err
	}
	log.Debug("get verifier nodes from staking", "verifierCount", len(verifierList))

	//if current block is a settlement block, to accumulate current verifiers for each voting proposal.
	if xutil.IsSettlementPeriod(blockNumber) {
		log.Debug("current block is at end of a settlement", "blockNumber", blockNumber, "blockHash", blockHash)
		for _, votingProposalID := range votingProposalIDs {
			if err := govPlugin.govDB.AccuVerifiers(blockHash, votingProposalID, verifierList); err != nil {
				return err
			}
		}
		//According to the proposal's rules, the settlement block must not be the end-voting block, so, just return.
		return nil
	}
	//iterate each voting proposal, to check if current block is proposal's end-voting block.
	for _, votingProposalID := range votingProposalIDs {
		log.Debug("iterate each voting proposal", "proposalID", votingProposalID)
		votingProposal, err := govPlugin.govDB.GetExistProposal(votingProposalID, state)
		if nil != err {
			return err
		}
		//todo:make sure blockNumber=N * ConsensusSize() - ElectionDistance
		if votingProposal.GetEndVotingBlock() == blockNumber {
			log.Debug("current block is end-voting block", "proposalID", votingProposal.GetProposalID(), "blockNumber", blockNumber)
			//According to the proposal's rules, the end-voting block must not at end of a settlement, so, to accumulate current verifiers for current voting proposal.
			if err := govPlugin.govDB.AccuVerifiers(blockHash, votingProposalID, verifierList); err != nil {
				return err
			}
			//tally the results
			if votingProposal.GetProposalType() == gov.Text {
				_, err := govPlugin.tallyText(votingProposal.GetProposalID(), blockHash, blockNumber, state)
				if err != nil {
					return err
				}
			} else if votingProposal.GetProposalType() == gov.Version {
				err = govPlugin.tallyVersion(votingProposal.(gov.VersionProposal), blockHash, blockNumber, state)
				if err != nil {
					return err
				}
			} else if votingProposal.GetProposalType() == gov.Cancel {
				_, err := govPlugin.tallyCancel(votingProposal.(gov.CancelProposal), blockHash, blockNumber, state)
				if err != nil {
					return err
				}
			} else {
				log.Error("invalid proposal type", "type", votingProposal.GetProposalType())
				err = errors.New("invalid proposal type")
				return err
			}
		}
	}
	return nil
}

// nil is allowed
func (govPlugin *GovPlugin) GetPreActiveVersion(state xcom.StateDB) uint32 {
	if nil == govPlugin {
		log.Error("The gov instance is nil on GetPreActiveVersion")
		return 0
	}
	if nil == govPlugin.govDB {
		log.Error("The govDB instance is nil on GetPreActiveVersion")
		return 0
	}
	return govPlugin.govDB.GetPreActiveVersion(state)
}

// should not be a nil value
func (govPlugin *GovPlugin) GetCurrentActiveVersion(state xcom.StateDB) uint32 {
	if nil == govPlugin {
		log.Error("The gov instance is nil on GetCurrentActiveVersion")
		return 0
	}
	if nil == govPlugin.govDB {
		log.Error("The govDB instance is nil on GetCurrentActiveVersion")
		return 0
	}

	return govPlugin.govDB.GetCurrentActiveVersion(state)
}

func (govPlugin *GovPlugin) GetActiveVersion(blockNumber uint64, state xcom.StateDB) uint32 {
	if nil == govPlugin {
		log.Error("The gov instance is nil on GetCurrentActiveVersion")
		return 0
	}
	if nil == govPlugin.govDB {
		log.Error("The govDB instance is nil on GetCurrentActiveVersion")
		return 0
	}

	avList, err := govPlugin.govDB.ListActiveVersion(state)
	if err != nil {
		log.Error("List active version error", "err", err)
		return 0
	}

	for _, av := range avList {
		if blockNumber >= av.ActiveBlock {
			return av.ActiveVersion
		}
	}
	return 0
}

func (govPlugin *GovPlugin) GetProgramVersion() (*gov.ProgramVersionValue, error) {
	if nil == govPlugin {
		log.Error("The gov instance is nil on GetProgramVersion")
		return nil, common.NewSysError("GovPlugin instance is nil")
	}
	if nil == govPlugin.govDB {
		log.Error("The govDB instance is nil on GetProgramVersion")
		return nil, common.NewSysError("GovDB instance is nil")
	}

	programVersion := uint32(params.VersionMajor<<16 | params.VersionMinor<<8 | params.VersionPatch)

	sig, err := xcom.GetCryptoHandler().Sign(programVersion)
	if err != nil {
		log.Error("sign version data error")
		return nil, err
	}

	value := &gov.ProgramVersionValue{ProgramVersion: programVersion, ProgramVersionSign: common.BytesToVersionSign(sig)}

	return value, nil
}

// submit a proposal
func (govPlugin *GovPlugin) Submit(from common.Address, proposal gov.Proposal, blockHash common.Hash, blockNumber uint64, state xcom.StateDB) error {
	log.Debug("call Submit", "from", from, "blockHash", blockHash, "blockNumber", blockNumber, "proposal", proposal)

	//param check
	if err := proposal.Verify(blockNumber, blockHash, state); err != nil {
		log.Error("verify proposal parameters failed", "err", err)
		return common.NewBizError(err.Error())
	}

	//check caller and proposer
	if err := govPlugin.checkVerifier(from, proposal.GetProposer(), blockHash, proposal.GetSubmitBlock()); err != nil {
		return err
	}

	//handle storage
	if err := govPlugin.govDB.SetProposal(proposal, state); err != nil {
		log.Error("save proposal failed", "proposalID", proposal.GetProposalID())
		return err
	}
	if err := govPlugin.govDB.AddVotingProposalID(blockHash, proposal.GetProposalID()); err != nil {
		log.Error("add proposal ID to voting proposal ID list failed", "proposalID", proposal.GetProposalID())
		return err
	}
	return nil
}

// vote for a proposal
func (govPlugin *GovPlugin) Vote(from common.Address, vote gov.Vote, blockHash common.Hash, blockNumber uint64, programVersion uint32, programVersionSign common.VersionSign, state xcom.StateDB) error {
	log.Debug("call Vote", "from", from, "blockHash", blockHash, "blockNumber", blockNumber, "programVersion", programVersion, "programVersionSign", programVersionSign, "voteInfo", vote)
	if vote.ProposalID == common.ZeroHash || vote.VoteOption == 0 {
		return common.NewBizError("empty parameter detected.")
	}

	proposal, err := govPlugin.govDB.GetProposal(vote.ProposalID, state)
	if err != nil {
		log.Error("cannot find proposal by ID", "proposalID", vote.ProposalID)
		return err
	} else if proposal == nil {
		log.Error("incorrect proposal ID.", "proposalID", vote.ProposalID)
		return common.NewBizError("incorrect proposal ID.")
	}

	//check caller and voter
	if err := govPlugin.checkVerifier(from, vote.VoteNodeID, blockHash, blockNumber); err != nil {
		return err
	}

	//voteOption range check
	if !(vote.VoteOption >= gov.Yes && vote.VoteOption <= gov.Abstention) {
		return common.NewBizError("vote option is error.")
	}

	if proposal.GetProposalType() == gov.Version {
		if vp, ok := proposal.(gov.VersionProposal); ok {
			//The signature should be verified when node vote for a version proposal.
			if !xcom.GetCryptoHandler().IsSignedByNodeID(programVersion, programVersionSign.Bytes(), vote.VoteNodeID) {
				return common.NewBizError("version sign error.")
			}

			//vote option can only be Yes for version proposal
			if vote.VoteOption != gov.Yes {
				return common.NewBizError("vote option error.")
			}

			if vp.GetNewVersion() != programVersion {
				log.Error("cannot vote for version proposal until node upgrade to a new version", "newVersion", vp.GetNewVersion(), "programVersion", programVersion)
				return common.NewBizError("node have not upgraded to a new version")
			}
		}
	}

	//check if vote.proposalID is in voting
	votingIDs, err := govPlugin.listVotingProposalID(blockHash, blockNumber, state)
	if err != nil {
		log.Error("list all voting proposal IDs failed", "blockHash", blockHash)
		return err
	} else if len(votingIDs) == 0 {
		log.Error("there's no voting proposal ID.", "blockHash", blockHash)
		return err
	} else {
		var isVoting = false
		for _, votingID := range votingIDs {
			if votingID == vote.ProposalID {
				isVoting = true
			}
		}
		if !isVoting {
			log.Error("proposal is not at voting stage", "proposalID", vote.ProposalID)
			return common.NewBizError("Proposal is not at voting stage.")
		}
	}

	//check if node has voted
	verifierList, err := govPlugin.govDB.ListVotedVerifier(vote.ProposalID, state)
	if err != nil {
		log.Error("list voted verifiers failed", "proposalID", vote.ProposalID)
		return err
	}

	if xutil.InNodeIDList(vote.VoteNodeID, verifierList) {
		log.Error("node has voted this proposal", "proposalID", vote.ProposalID, "nodeID", byteutil.PrintNodeID(vote.VoteNodeID))
		return common.NewBizError("node has voted this proposal.")
	}

	//handle storage
	if err := govPlugin.govDB.SetVote(vote.ProposalID, vote.VoteNodeID, vote.VoteOption, state); err != nil {
		log.Error("save vote failed", "proposalID", vote.ProposalID)
		return err
	}

	//the proposal is version type, so add the node ID to active node list.
	if proposal.GetProposalType() == gov.Version {
		if err := govPlugin.govDB.AddActiveNode(blockHash, vote.ProposalID, vote.VoteNodeID); err != nil {
			log.Error("add nodeID to active node list failed", "proposalID", vote.ProposalID, "nodeID", byteutil.PrintNodeID(vote.VoteNodeID))
			return err
		}
	}

	return nil
}

// node declares it's version
func (govPlugin *GovPlugin) DeclareVersion(from common.Address, declaredNodeID discover.NodeID, declaredVersion uint32, programVersionSign common.VersionSign, blockHash common.Hash, blockNumber uint64, state xcom.StateDB) error {
	log.Debug("call DeclareVersion", "from", from, "blockHash", blockHash, "blockNumber", blockNumber, "declaredNodeID", declaredNodeID, "declaredVersion", declaredVersion, "versionSign", programVersionSign)
	//check caller is a Verifier or Candidate
	/*if err := govPlugin.checkVerifier(from, declaredNodeID, blockHash, blockNumber); err != nil {
		return err
	}*/

	if !xcom.GetCryptoHandler().IsSignedByNodeID(declaredVersion, programVersionSign.Bytes(), declaredNodeID) {
		return common.NewBizError("version sign error.")
	}

	if err := govPlugin.checkCandidate(from, declaredNodeID, blockHash, blockNumber); err != nil {
		return err
	}

	activeVersion := uint32(govPlugin.GetCurrentActiveVersion(state))
	if activeVersion <= 0 {
		return common.NewBizError("wrong current active version.")
	}

	votingVP, err := govPlugin.govDB.FindVotingVersionProposal(blockHash, blockNumber, state)
	if err != nil {
		log.Error("find if there's a voting version proposal failed", "blockHash", blockHash)
		return err
	}

	//there is a voting version proposal
	if votingVP != nil {
		if declaredVersion>>8 == activeVersion>>8 {
			nodeList, err := govPlugin.govDB.ListVotedVerifier(votingVP.ProposalID, state)
			if err != nil {
				log.Error("cannot list voted verifiers", "proposalID", votingVP.ProposalID)
				return err
			} else {
				if xutil.InNodeIDList(declaredNodeID, nodeList) && declaredVersion != votingVP.GetNewVersion() {
					log.Error("declared version should be new version",
						"declaredNodeID", declaredNodeID, "declaredVersion", declaredVersion, "proposalID", votingVP.ProposalID, "newVersion", votingVP.GetNewVersion())
					return common.NewBizError("declared version should be same as proposal's version")
				} else {
					//there's a voting-version-proposal, if the declared version equals the current active version, notify staking immediately
					log.Debug("there's a voting-version-proposal, and declared version equals active version, notify staking immediately.",
						"blockNumber", blockNumber, "declaredNodeID", declaredNodeID, "declaredVersion", declaredVersion, "activeVersion", activeVersion)
					if err := stk.DeclarePromoteNotify(blockHash, blockNumber, declaredNodeID, declaredVersion); err != nil {
						log.Error("notify staking of declared node ID failed", "err", err)
						return common.NewBizError("notify staking of declared node ID failed")
					}
				}
			}
		} else if declaredVersion>>8 == votingVP.GetNewVersion()>>8 {
			//the declared version equals the new version, will notify staking when the proposal is passed
			log.Debug("declared version equals the new version.",
				"newVersion", votingVP.GetNewVersion, "declaredVersion", declaredVersion)
			if err := govPlugin.govDB.AddActiveNode(blockHash, votingVP.ProposalID, declaredNodeID); err != nil {
				log.Error("add declared node ID to active node list failed", "err", err)
				return err
			}
		} else {
			log.Error("declared version neither equals active version nor new version.", "activeVersion", activeVersion, "newVersion", votingVP.GetNewVersion, "declaredVersion", declaredVersion)
			return common.NewBizError("declared version neither equals active version nor new version.")
		}
	} else {
		preActiveVersion := govPlugin.govDB.GetPreActiveVersion(state)
		if declaredVersion>>8 == activeVersion>>8 || (preActiveVersion != 0 && declaredVersion == preActiveVersion) {
			//there's no voting-version-proposal, if the declared version equals either the current active version or preActive version, notify staking immediately
			//stk.DeclarePromoteNotify(blockHash, blockNumber, declaredNodeID, declaredVersion)
			log.Debug("there's no voting-version-proposal, the declared version equals either the current active version or preActive version, notify staking immediately.",
				"blockNumber", blockNumber, "declaredVersion", declaredVersion, "declaredNodeID", declaredNodeID, "activeVersion", activeVersion, "preActiveVersion", preActiveVersion)
			if err := stk.DeclarePromoteNotify(blockHash, blockNumber, declaredNodeID, declaredVersion); err != nil {
				log.Error("notify staking of declared node ID failed", "err", err)
				return common.NewBizError("notify staking of declared node ID failed")
			}
		} else {
			log.Error("there's no version proposal at voting stage, declared version should be active or pre-active version.", "activeVersion", activeVersion, "declaredVersion", declaredVersion)
			return common.NewBizError("there's no version proposal at voting stage, declared version should be active version.")
		}
	}
	return nil
}

// client query a specified proposal
func (govPlugin *GovPlugin) GetProposal(proposalID common.Hash, state xcom.StateDB) (gov.Proposal, error) {
	log.Debug("call GetProposal", "proposalID", proposalID)

	proposal, err := govPlugin.govDB.GetProposal(proposalID, state)
	if err != nil {
		log.Error("get proposal by ID failed", "proposalID", proposalID, "msg", err.Error())
		return nil, err
	}
	if proposal == nil {
		return nil, common.NewBizError("incorrect proposal ID.")
	}
	return proposal, nil
}

// query a specified proposal's tally result
func (govPlugin *GovPlugin) GetTallyResult(proposalID common.Hash, state xcom.StateDB) (*gov.TallyResult, error) {
	tallyResult, err := govPlugin.govDB.GetTallyResult(proposalID, state)
	if err != nil {
		log.Error("get tallyResult by proposal ID failed.", "proposalID", proposalID, "msg", err.Error())
		return nil, err
	}
	if nil == tallyResult {
		return nil, common.NewBizError("get tallyResult by proposal ID failed.")
	}

	return tallyResult, nil
}

// query proposal list
func (govPlugin *GovPlugin) ListProposal(blockHash common.Hash, state xcom.StateDB) ([]gov.Proposal, error) {
	log.Debug("call ListProposal")
	var proposalIDs []common.Hash
	var proposals []gov.Proposal

	votingProposals, err := govPlugin.govDB.ListVotingProposal(blockHash)
	if err != nil {
		log.Error("list voting proposals failed.", "blockHash", blockHash)
		return nil, err
	}
	endProposals, err := govPlugin.govDB.ListEndProposalID(blockHash)
	if err != nil {
		log.Error("list end proposals failed.", "blockHash", blockHash)
		return nil, err
	}

	preActiveProposals, err := govPlugin.govDB.GetPreActiveProposalID(blockHash)
	if err != nil {
		log.Error("find pre-active proposal failed.", "blockHash", blockHash)
		return nil, err
	}

	proposalIDs = append(proposalIDs, votingProposals...)
	proposalIDs = append(proposalIDs, endProposals...)
	if preActiveProposals != common.ZeroHash {
		proposalIDs = append(proposalIDs, preActiveProposals)
	}

	for _, proposalID := range proposalIDs {
		proposal, err := govPlugin.govDB.GetExistProposal(proposalID, state)
		if err != nil {
			log.Error("find proposal failed.", "proposalID", proposalID)
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	return proposals, nil
}

// tally a version proposal
func (govPlugin *GovPlugin) tallyVersion(proposal gov.VersionProposal, blockHash common.Hash, blockNumber uint64, state xcom.StateDB) error {
	proposalID := proposal.ProposalID
	log.Debug("call tallyForVersionProposal", "blockHash", blockHash, "blockNumber", blockNumber, "proposalID", proposal.ProposalID)

	verifiersCnt, err := govPlugin.govDB.AccuVerifiersLength(blockHash, proposalID)
	if err != nil {
		log.Error("count accumulated verifiers failed", blockNumber, "blockHash", blockHash, "proposalID", proposalID, "blockNumber")
		return err
	}

	voteList, err := govPlugin.govDB.ListVoteValue(proposalID, state)
	if err != nil {
		log.Error("list voted values failed", "blockNumber", blockNumber, "blockHash", blockHash, "proposalID", proposalID)
		return err
	}

	voteCnt := uint16(len(voteList))
	yeas := voteCnt //`voteOption` can be ignored in version proposal, set voteCount to passCount as default.

	status := gov.Failed
	supportRate := float64(yeas) / float64(verifiersCnt)
	log.Debug("version proposal's supportRate", "supportRate", supportRate, "voteCount", voteCnt, "verifierCount", verifiersCnt)

	if supportRate >= xcom.VersionProposal_SupportRate() {
		status = gov.PreActive

		if err := govPlugin.govDB.MoveVotingProposalIDToPreActive(blockHash, proposalID); err != nil {
			log.Error("move version proposal ID to pre-active failed", "blockNumber", blockNumber, "blockHash", blockHash, "proposalID", proposalID)
			return err
		}

		if err := govPlugin.govDB.SetPreActiveVersion(proposal.NewVersion, state); err != nil {
			log.Error("save pre-active version to state failed", "blockHash", blockHash, "proposalID", proposalID, "newVersion", proposal.NewVersion)
			return err
		}

		activeList, err := govPlugin.govDB.GetActiveNodeList(blockHash, proposalID)
		if err != nil {
			log.Error("list active nodes failed", "blockNumber", blockNumber, "blockHash", blockHash, "proposalID", proposalID)
			return err
		}
		if err := stk.ProposalPassedNotify(blockHash, blockNumber, activeList, proposal.NewVersion); err != nil {
			log.Error("notify stating of the upgraded node list failed", "blockHash", blockHash, "proposalID", proposalID, "newVersion", proposal.NewVersion, "activeList", activeList)
			return err
		}

	} else {
		status = gov.Failed
		if err := govPlugin.govDB.MoveVotingProposalIDToEnd(blockHash, proposalID, state); err != nil {
			log.Error("move proposalID from voting proposalID list to end list failed", "blockHash", blockHash, "proposalID", proposalID)
			return err
		}
	}

	tallyResult := &gov.TallyResult{
		ProposalID:    proposalID,
		Yeas:          yeas,
		Nays:          0x0,
		Abstentions:   0x0,
		AccuVerifiers: verifiersCnt,
		Status:        status,
	}

	log.Debug("version proposal tally result", "tallyResult", tallyResult)
	if err := govPlugin.govDB.SetTallyResult(*tallyResult, state); err != nil {
		log.Error("save tally result failed", "tallyResult", tallyResult)
		return err
	}
	return nil
}

func (govPlugin *GovPlugin) tallyText(proposalID common.Hash, blockHash common.Hash, blockNumber uint64, state xcom.StateDB) (pass bool, err error) {
	return govPlugin.tally(gov.Text, proposalID, blockHash, blockNumber, state)
}

func (govPlugin *GovPlugin) tallyCancel(cp gov.CancelProposal, blockHash common.Hash, blockNumber uint64, state xcom.StateDB) (pass bool, err error) {
	if pass, err := govPlugin.tally(gov.Cancel, cp.ProposalID, blockHash, blockNumber, state); err != nil {
		return false, err
	} else if pass {
		if proposal, err := govPlugin.govDB.GetExistProposal(cp.TobeCanceled, state); err != nil {
			return false, err
		} else if proposal.GetProposalType() != gov.Version {
			return false, common.NewBizError("Tobe canceled proposal is not a version proposal.")
		}
		if votingProposalIDList, err := govPlugin.listVotingProposalID(blockHash, blockNumber, state); err != nil {
			return false, err
		} else if !xutil.InHashList(cp.TobeCanceled, votingProposalIDList) {
			return false, common.NewBizError("Tobe canceled proposal is not at voting stage.")
		}

		if tallyResult, err := govPlugin.GetTallyResult(cp.TobeCanceled, state); err != nil {
			return false, err
		} else {
			tallyResult.Status = gov.Canceled
			tallyResult.CanceledBy = cp.ProposalID

			log.Debug("version proposal is canceled by other", "tallyResult", tallyResult)
			if err := govPlugin.govDB.SetTallyResult(*tallyResult, state); err != nil {
				log.Error("save tally result failed", "tallyResult", tallyResult)
				return false, err
			}

			if err := govPlugin.govDB.ClearActiveNodes(blockHash, cp.TobeCanceled); err != nil {
				return false, err
			}

			if err := govPlugin.govDB.MoveVotingProposalIDToEnd(blockHash, cp.TobeCanceled, state); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func (govPlugin *GovPlugin) tally(proposalType gov.ProposalType, proposalID common.Hash, blockHash common.Hash, blockNumber uint64, state xcom.StateDB) (pass bool, err error) {
	log.Debug("call tallyBasic", "blockHash", blockHash, "blockNumber", blockNumber, "proposalID", proposalID)

	verifiersCnt, err := govPlugin.govDB.AccuVerifiersLength(blockHash, proposalID)
	if err != nil {
		log.Error("count accumulated verifiers failed", "proposalID", proposalID, "blockHash", blockHash)
		return false, err
	}

	status := gov.Voting
	yeas := uint16(0)
	nays := uint16(0)
	abstentions := uint16(0)

	voteList, err := govPlugin.govDB.ListVoteValue(proposalID, state)
	if err != nil {
		log.Error("list voted value failed.", "blockHash", blockHash)
		return false, err
	}
	for _, v := range voteList {
		if v.VoteOption == gov.Yes {
			yeas++
		}
		if v.VoteOption == gov.No {
			nays++
		}
		if v.VoteOption == gov.Abstention {
			abstentions++
		}
	}
	voteRate := float64(yeas + nays + abstentions/verifiersCnt)
	supportRate := float64(yeas / verifiersCnt)

	switch proposalType {
	case gov.Text:
		if voteRate > xcom.TextProposal_VoteRate() && supportRate > xcom.TextProposal_SupportRate() {
			status = gov.Pass
		} else {
			status = gov.Failed
		}
	case gov.Cancel:
		if voteRate > xcom.CancelProposal_VoteRate() && supportRate > xcom.CancelProposal_SupportRate() {
			status = gov.Pass
		} else {
			status = gov.Failed
		}
	}

	tallyResult := &gov.TallyResult{
		ProposalID:    proposalID,
		Yeas:          yeas,
		Nays:          nays,
		Abstentions:   abstentions,
		AccuVerifiers: verifiersCnt,
		Status:        status,
	}

	//govPlugin.govDB.MoveVotingProposalIDToEnd(blockHash, proposalID, state)
	if err := govPlugin.govDB.MoveVotingProposalIDToEnd(blockHash, proposalID, state); err != nil {
		log.Error("move proposalID from voting proposalID list to end list failed", "blockHash", blockHash, "proposalID", proposalID)
		return false, err
	}

	log.Debug("proposal tally result", "tallyResult", tallyResult)

	if err := govPlugin.govDB.SetTallyResult(*tallyResult, state); err != nil {
		log.Error("save tally result failed", "tallyResult", tallyResult)
		return false, err
	}
	return status == gov.Pass, nil
}

// check if the node a verifier, and the caller address is same as the staking address
func (govPlugin *GovPlugin) checkVerifier(from common.Address, nodeID discover.NodeID, blockHash common.Hash, blockNumber uint64) error {
	log.Debug("call checkVerifier", "from", from, "blockHash", blockHash, "blockNumber", blockNumber, "nodeID", nodeID)
	verifierList, err := stk.GetVerifierList(blockHash, blockNumber, QueryStartNotIrr)
	if err != nil {
		log.Error("list verifiers failed", "blockHash", blockHash, "err", err)
		return err
	}

	for _, verifier := range verifierList {
		if verifier != nil && verifier.NodeId == nodeID {
			if verifier.StakingAddress == from {
				nodeAddress, _ := xutil.NodeId2Addr(verifier.NodeId)
				candidate, err := stk.GetCandidateInfo(blockHash, nodeAddress)
				if err != nil {
					return common.NewBizError("cannot get verifier's detail info.")
				} else if staking.Is_Invalid(candidate.Status) {
					return common.NewBizError("verifier's status is invalid.")
				}
				log.Debug("tx sender is a valid verifier.", "from", from, "blockHash", blockHash, "blockNumber", blockNumber, "nodeID", nodeID)
				return nil
			} else {
				return common.NewBizError("tx sender should be node's staking address.")
			}
		}
	}
	log.Error("tx sender is not a verifier.", "from", from, "blockHash", blockHash, "blockNumber", blockNumber, "nodeID", nodeID)
	return common.NewBizError("tx sender is not a verifier.")
}

// check if the node a candidate, and the caller address is same as the staking address
func (govPlugin *GovPlugin) checkCandidate(from common.Address, nodeID discover.NodeID, blockHash common.Hash, blockNumber uint64) error {
	log.Debug("call checkCandidate", "from", from, "blockHash", blockHash, "blockNumber", blockNumber, "nodeID", nodeID)
	candidateList, err := stk.GetCandidateList(blockHash, blockNumber)
	if err != nil {
		log.Error("list candidates failed", "blockHash", blockHash)
		return err
	}

	for _, candidate := range candidateList {
		if candidate.NodeId == nodeID {
			if candidate.StakingAddress == from {
				log.Debug("tx sender is a candidate.", "from", from, "blockHash", blockHash, "blockNumber", blockNumber, "nodeID", nodeID)
				return nil
			} else {
				return common.NewBizError("tx sender should be node's staking address.")
			}
		}
	}
	return common.NewBizError("tx sender is not candidate.")
}

// list all proposal IDs at voting stage
func (govPlugin *GovPlugin) listVotingProposalID(blockHash common.Hash, blockNumber uint64, state xcom.StateDB) ([]common.Hash, error) {
	log.Debug("call checkCandidate", "blockHash", blockHash, "blockNumber", blockNumber)
	idList, err := govPlugin.govDB.ListVotingProposal(blockHash)
	if err != nil {
		log.Error("find voting version proposal failed", "blockHash", blockHash)
		return nil, err
	}
	return idList, nil
}