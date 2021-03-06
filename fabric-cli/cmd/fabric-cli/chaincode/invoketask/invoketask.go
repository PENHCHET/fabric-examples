/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package invoketask

import (
	"time"

	"github.com/hyperledger/fabric-sdk-go/api/apifabclient"
	"github.com/hyperledger/fabric-sdk-go/api/apitxn"
	pb "github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/peer"
	"github.com/securekey/fabric-examples/fabric-cli/cmd/fabric-cli/action"
	"github.com/securekey/fabric-examples/fabric-cli/cmd/fabric-cli/chaincode/invokeerror"
	"github.com/securekey/fabric-examples/fabric-cli/cmd/fabric-cli/chaincode/responsefilter"
	"github.com/securekey/fabric-examples/fabric-cli/cmd/fabric-cli/chaincode/utils"
	cliconfig "github.com/securekey/fabric-examples/fabric-cli/cmd/fabric-cli/config"
	"github.com/securekey/fabric-examples/fabric-cli/cmd/fabric-cli/executor"
	"github.com/securekey/fabric-examples/fabric-cli/cmd/fabric-cli/printer"
)

// Task is a Task that invokes a chaincode
type Task struct {
	executor      *executor.Executor
	channelClient apitxn.ChannelClient
	id            string
	ccID          string
	args          *action.ArgStruct
	maxAttempts   int
	resubmitDelay time.Duration
	attempt       int
	lastErr       error
	callback      func(err error)
	verbose       bool
	printer       printer.Printer
	txID          string
}

// New returns a new Task
func New(id string, channelClient apitxn.ChannelClient, ccID string, args *action.ArgStruct,
	executor *executor.Executor, maxAttempts int, resubmitDelay time.Duration, verbose bool,
	p printer.Printer, callback func(err error)) *Task {
	return &Task{
		id:            id,
		channelClient: channelClient,
		printer:       p,
		ccID:          ccID,
		args:          args,
		executor:      executor,
		maxAttempts:   maxAttempts,
		callback:      callback,
		attempt:       1,
		resubmitDelay: resubmitDelay,
		verbose:       verbose,
	}
}

// Attempts returns the number of invocation attempts that were made
// in order to achieve a successful response
func (t *Task) Attempts() int {
	return t.attempt
}

// LastError returns the last error that was recorder
func (t *Task) LastError() error {
	return t.lastErr
}

// Invoke invokes the task
func (t *Task) Invoke() {
	if err := t.doInvoke(); err != nil {
		t.lastErr = err
		invokeErr := err.(invokeerror.Error)
		if invokeErr != nil {
			switch invokeErr.ErrorCode() {
			case invokeerror.TransientError:
				if t.attempt < t.maxAttempts {
					cliconfig.Config().Logger().Debugf("(%s) - Error invoking chaincode: %s. Resubmitting ...\n", t.id, err)
					t.attempt++
					if err := t.executor.SubmitDelayed(t, t.resubmitDelay); err != nil {
						cliconfig.Config().Logger().Errorf("error submitting task: %s", err)
					}
					return
				}
				cliconfig.Config().Logger().Debugf("(%s) - Error invoking chaincode: %s. Giving up after %d attempts.\n", t.id, err, t.attempt)
			case invokeerror.TimeoutOnCommit:
				cliconfig.Config().Logger().Debugf("(%s) - Timeout committing Tx %s\n", t.id, t.txID)
				// TODO: Handle somehow?
			}
		}
		t.callback(err)
	} else {
		cliconfig.Config().Logger().Debugf("(%s) - Successfully invoked chaincode\n", t.id)
		t.callback(nil)
	}
}

func (t *Task) doInvoke() error {
	cliconfig.Config().Logger().Debugf("(%s) - Invoking chaincode: %s, function: %s, args: %+v. Attempt #%d...\n",
		t.id, t.ccID, t.args.Func, t.args.Args, t.attempt)

	txStatusEvents := make(chan apitxn.ExecuteTxResponse)
	txnID, err := t.channelClient.ExecuteTxWithOpts(
		apitxn.ExecuteTxRequest{
			ChaincodeID: t.ccID,
			Fcn:         t.args.Func,
			Args:        utils.AsBytes(t.args.Args),
		},
		apitxn.ExecuteTxOpts{
			TxFilter: responsefilter.New(t.verbose, t.printer),
			Notifier: txStatusEvents,
			// ProposalProcessors: asProposalProcessors(t.targets),
			Timeout: cliconfig.Config().Timeout(),
		},
	)
	if err != nil {
		return invokeerror.Errorf(invokeerror.TransientError, "SendTransactionProposal return error: %v", err)
	}

	t.txID = txnID.ID

	cliconfig.Config().Logger().Debugf("(%s) - Committing transaction - TxID [%s] ...\n", t.id, txnID.ID)

	select {
	case s := <-txStatusEvents:
		switch s.TxValidationCode {
		case pb.TxValidationCode_VALID:
			cliconfig.Config().Logger().Debugf("(%s) - Successfully committed transaction [%s] ...\n", t.id, txnID.ID)
			return nil
		case pb.TxValidationCode_DUPLICATE_TXID, pb.TxValidationCode_MVCC_READ_CONFLICT, pb.TxValidationCode_PHANTOM_READ_CONFLICT:
			cliconfig.Config().Logger().Debugf("(%s) - Transaction commit failed for [%s] with code [%s]. This is most likely a transient error.\n", t.id, txnID.ID, s.TxValidationCode)
			return invokeerror.Wrapf(invokeerror.TransientError, s.Error, "invoke Error received from eventhub for TxID [%s]. Code: %s", txnID.ID, s.TxValidationCode)
		default:
			cliconfig.Config().Logger().Debugf("(%s) - Transaction commit failed for [%s] with code [%s].\n", t.id, txnID.ID, s.TxValidationCode)
			return invokeerror.Wrapf(invokeerror.PersistentError, s.Error, "invoke Error received from eventhub for TxID [%s]. Code: %s", txnID.ID, s.TxValidationCode)
		}
	case <-time.After(cliconfig.Config().Timeout()):
		return invokeerror.Errorf(invokeerror.TimeoutOnCommit, "timed out waiting to receive block event for TxID [%s]", txnID.ID)
	}
}

func asProposalProcessors(peers []apifabclient.Peer) []apitxn.ProposalProcessor {
	targets := make([]apitxn.ProposalProcessor, len(peers))
	for i, p := range peers {
		targets[i] = p
	}
	return targets
}
