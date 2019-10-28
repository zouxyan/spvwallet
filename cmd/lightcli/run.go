package main

import (
	"encoding/hex"
	"fmt"
	"github.com/ontio/spvwallet"
	"github.com/ontio/spvwallet/alliance"
	"github.com/ontio/spvwallet/log"
	"github.com/ontio/spvwallet/rest/http/restful"
	"github.com/ontio/spvwallet/rest/service"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/google/gops/agent"
	sdk "github.com/ontio/multi-chain-go-sdk"
	"github.com/ontio/multi-chain/native/service/cross_chain_manager/btc"
	"github.com/urfave/cli"
	"net"
	"os"
	"os/signal"
	"path"
	"runtime"
	"syscall"
	"time"
)

func setupApp() *cli.App {
	app := cli.NewApp()
	app.Usage = "start spv client"
	app.Action = startSpvClient
	app.Copyright = ""
	app.Flags = []cli.Flag{
		spvwallet.LogLevelFlag,
		spvwallet.ConfigBitcoinNet,
		spvwallet.ConfigDBPath,
		spvwallet.TrustedPeer,
		spvwallet.AlliaConfigFile,
		spvwallet.GoMaxProcs,
		spvwallet.RunRest,
		spvwallet.RestConfigPathFlag,
		spvwallet.RunVote,
		spvwallet.RestartDuration,
		spvwallet.IsRestart,
	}
	app.Before = func(context *cli.Context) error {
		cores := context.GlobalInt(spvwallet.GoMaxProcs.Name)
		runtime.GOMAXPROCS(cores)
		return nil
	}
	return app
}

func startSpvClient(ctx *cli.Context) {
	logLevel := ctx.GlobalInt(spvwallet.GetFlagName(spvwallet.LogLevelFlag))
	log.InitLog(logLevel, log.Stdout)

	conf := spvwallet.NewDefaultConfig()
	isVote := ctx.GlobalInt(spvwallet.RunVote.Name) == 1
	conf.IsVote = isVote

	netType := ctx.GlobalString(spvwallet.GetFlagName(spvwallet.ConfigBitcoinNet))
	dbPath := ctx.GlobalString(spvwallet.GetFlagName(spvwallet.ConfigDBPath))
	if dbPath != "" {
		conf.RepoPath = dbPath
	}

	switch netType {
	case "regtest":
		conf.Params = &chaincfg.RegressionNetParams
		conf.RepoPath = path.Join(conf.RepoPath, "regtest")
	case "test":
		conf.Params = &chaincfg.TestNet3Params
	case "sim":
		conf.Params = &chaincfg.SimNetParams
	default:
		conf.Params = &chaincfg.MainNetParams
	}

	tp := ctx.GlobalString(spvwallet.GetFlagName(spvwallet.TrustedPeer))
	if tp != "" {
		conf.TrustedPeer, _ = net.ResolveTCPAddr("tcp", tp+":"+conf.Params.DefaultPort)
	}

	wallet, err1 := spvwallet.NewSPVWallet(conf)
	if err1 != nil {
		log.Errorf("Failed to new a wallet: %v", err1)
		os.Exit(1)
	}
	wallet.Start()
	defer wallet.Close()

	//var restServer restful.ApiServer
	var err error
	if ctx.GlobalInt(spvwallet.RunRest.Name) == 1 {
		_, err = startServer(ctx, wallet)
		if err != nil {
			log.Fatalf("Failed to start rest service: %v", err)
			os.Exit(1)
		}
	}

	//var ob *alliance.Observer
	voting := make(chan *btc.BtcProof, 10)
	txchan := make(chan *alliance.ToSignItem, 10)
	//var voter *alliance.Voter
	if isVote {
		_, _, err = startAllianceService(ctx, wallet, voting, txchan, conf.Params)
		if err != nil {
			log.Fatalf("Failed to start alliance service: %v", err)
		}
	}

	sh, err := wallet.Blockchain.BestBlock()
	if err != nil {
		log.Fatalf("Failed to get best block: %v", err)
		os.Exit(1)
	}
	lasth := sh.Height

	if ctx.GlobalInt(spvwallet.IsRestart.Name) == 1 {
		again := false
		td := time.Duration(ctx.GlobalInt(spvwallet.RestartDuration.Name)) * time.Minute
		timer := time.NewTimer(td)
		for {
			<-timer.C
			sh, err = wallet.Blockchain.BestBlock()
			if err != nil {
				log.Fatalf("Failed to get best block: %v", err)
				continue
			}
			if lasth >= sh.Height {
				isrb := false
				log.Debugf("Restart now!!!")
				//if isVote {
				//	log.Debugf("stop voter")
				//	voter.Stop()
				//}
				//if restServer != nil {
				//	log.Debugf("stop rest service")
				//	restServer.Stop()
				//}

				if again {
					log.Debugf("It happened TWICE!!!")
					err = wallet.Blockchain.Rollback(sh.Header.Timestamp.Add(-6 * time.Hour))
					if err != nil {
						log.Fatalf("Failed to rollback: %v", err)
						continue
					}
					isrb = true
					//_ = os.RemoveAll(path.Join(conf.RepoPath, "peers.json"))
				}

				wallet.ReSync()
				//wallet.Close()
				//
				//wallet, _ = spvwallet.NewSPVWallet(conf)
				//wallet.Start()

				//if ctx.GlobalInt(spvwallet.RunRest.Name) == 1 {
				//	restServer, err = startServer(ctx, wallet)
				//	if err != nil {
				//		log.Fatalf("Failed to restart rest service: %v", err)
				//		continue
				//	}
				//}

				//if isVote {
				//	voter.Restart(wallet)
				//}

				log.Info("The block header is not updated for a long time. Restart the service")
				if isrb {
					again = false
				} else {
					again = true
				}
				timer.Reset(td / 2)
			} else {
				again = false
				timer.Reset(td)
			}
			lasth = sh.Height
		}
	} else {
		waitToExit()
	}
}

func startServer(ctx *cli.Context, wallet *spvwallet.SPVWallet) (restful.ApiServer, error) {
	configPath := ctx.GlobalString(spvwallet.GetFlagName(spvwallet.RestConfigPathFlag))
	servConfig, err := spvwallet.NewRestConfig(configPath)
	if err != nil {
		return nil, err
	}

	serv := service.NewService(wallet, servConfig)
	restServer := restful.InitRestServer(serv, servConfig.Port)
	go restServer.Start()
	//go checkLogFile(logLevel)

	return restServer, nil
}

func startAllianceService(ctx *cli.Context, wallet *spvwallet.SPVWallet, voting chan *btc.BtcProof,
	txchan chan *alliance.ToSignItem, params *chaincfg.Params) (*alliance.Observer, *alliance.Voter, error) {
	conf, err := alliance.NewAlliaConfig(ctx.GlobalString(spvwallet.GetFlagName(spvwallet.AlliaConfigFile)))
	if err != nil {
		return nil, nil, err
	}

	allia := sdk.NewMultiChainSdk()
	allia.NewRpcClient().SetAddress(conf.AllianceJsonRpcAddress)
	acct, err := alliance.GetAccountByPassword(allia, conf.WalletFile, conf.WalletPwd)
	if err != nil {
		return nil, nil, fmt.Errorf("GetAccountByPassword failed: %v", err)
	}

	ob := alliance.NewObserver(allia, &alliance.ObConfig{
		FirstN:            conf.AlliaObFirstN,
		LoopWaitTime:      conf.AlliaObLoopWaitTime,
		WatchingKey:       conf.WatchingKey,
		WatchingMakeTxKey: conf.WatchingMakeTxKey,
	}, voting, txchan)
	go ob.Listen()

	redeem, err := hex.DecodeString(conf.Redeem)
	if err != nil {
		return ob, nil, fmt.Errorf("failed to decode redeem %s: %v", conf.Redeem, err)
	}
	v, err := alliance.NewVoter(allia, voting, wallet, redeem, acct, conf.GasPrice, conf.GasLimit, conf.WaitingDBPath,
		conf.BlksToWait)
	if err != nil {
		return ob, v, fmt.Errorf("failed to new a voter: %v", err)
	}

	go v.Vote()
	go v.WaitingRetry()

	signer, err := alliance.NewSigner(conf.BtcPrivk, txchan, acct, conf.GasPrice, conf.GasLimit, allia, params)
	if err != nil {
		return ob, v, fmt.Errorf("failed to new a signer: %v", err)
	}
	go signer.Signing()

	return ob, v, nil
}

func waitToExit() {
	exit := make(chan bool, 0)
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sc {
			log.Infof("server received exit signal:%v.", sig.String())
			close(exit)
			break
		}
	}()
	<-exit
}

func checkLogFile(logLevel int) {
	ticker := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-ticker.C:
			isNeedNewFile := log.CheckIfNeedNewFile()
			if isNeedNewFile {
				log.ClosePrintLog()
				log.InitLog(logLevel, log.PATH, log.Stdout)
			}
		}
	}
}

func main() {
	if err := agent.Listen(agent.Options{}); err != nil {
		log.Fatal(err)
	}

	if err := setupApp().Run(os.Args); err != nil {
		log.Errorf("fail to run: %v", err)
		os.Exit(1)
	}
}
