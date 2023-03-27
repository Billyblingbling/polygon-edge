package aarelayer

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/0xPolygon/polygon-edge/command/aarelayer/service"
	"github.com/0xPolygon/polygon-edge/command/helper"
	"github.com/0xPolygon/polygon-edge/command/polybftsecrets"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/wallet"
	"github.com/0xPolygon/polygon-edge/helper/common"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var params aarelayerParams

func GetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "aarelayer",
		Short:   "Account abstraction relayer",
		PreRunE: runPreRun,
		RunE:    runCommand,
	}

	setFlags(cmd)

	return cmd
}

func runPreRun(cmd *cobra.Command, _ []string) error {
	return params.validateFlags()
}

func runCommand(cmd *cobra.Command, _ []string) error {
	cmd.SilenceUsage = true
	cmd.SetOut(os.Stdout)

	state, err := service.NewAATxState(params.dbPath)
	if err != nil {
		return err
	}

	pending, err := state.GetAllPending()
	if err != nil {
		return err
	}

	config := service.DefaultConfig()

	jsonRPC := helper.GetJSONRPCAddress(cmd)
	if !strings.HasPrefix(jsonRPC, "http://") {
		jsonRPC = "http://" + jsonRPC
	}

	txSender, err := service.NewAATxSender(jsonRPC)
	if err != nil {
		return err
	}

	secretsManager, err := polybftsecrets.GetSecretsManager(params.accountDir, params.configPath, true)
	if err != nil {
		return err
	}

	account, err := wallet.NewAccountFromSecret(secretsManager)
	if err != nil {
		return err
	}

	logger, err := params.getLogger()
	if err != nil {
		return err
	}

	invokerAddress := types.StringToAddress(params.invokerAddr)

	pool := service.NewAAPool()
	pool.Init(pending)

	verification := service.NewAAVerification(
		config,
		invokerAddress,
		params.chainID,
		func(a *service.AATransaction) error { return nil })
	restService := service.NewAARelayerRestServer(pool, state, verification, logger)

	relayerService, err := service.NewAARelayerService(
		txSender,
		pool,
		state,
		account.Ecdsa,
		invokerAddress,
		logger,
		service.WithPullTime(config.PullTime),
		service.WithReceiptDelay(config.ReceiptRetryDelay),
		service.WithNumRetries(config.ReceiptNumRetries))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	stopCh := common.GetTerminationSignalCh()
	g := errgroup.Group{}

	// just waits for os.Signal to cancel context
	g.Go(func() error {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}

		return nil
	})

	// rest server for incoming requests
	g.Go(func() error {
		cmd.Printf("Rest server is listening on %s...\n", params.addr)

		if err := restService.ListenAndServe(params.addr); !errors.Is(err, http.ErrServerClosed) {
			cmd.PrintErrf("Rest server has been terminated with an error = %v\n", err)

			return err
		}

		cmd.Printf("Rest server has been terminated\n")

		cancel()

		return nil
	})

	// service which pools from state and send to jsonrpc of some node
	g.Go(func() error {
		relayerService.Start(ctx)
		cmd.Printf("AA relayer service has been terminated\n")

		if err := restService.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}

		cancel()

		return nil
	})

	return g.Wait()
}