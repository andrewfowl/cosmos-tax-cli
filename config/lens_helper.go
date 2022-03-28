package config

import (
	lensClient "github.com/strangelove-ventures/lens/client"
)

func GetLensClient(conf lens) *lensClient.ChainClient {
	//IMPORTANT: the actual keyring-test will be searched for at the path {homepath}/keys/{ChainID}/keyring-test.
	//You can use lens default settings to generate that directory appropriately then move it to the desired path.
	//For example, 'lens keys restore default' will restore the key to the default keyring (e.g. /home/kyle/.lens/...)
	//and you can move all of the necessary keys to whatever homepath you want to use. Or you can use --home flag.
	cl, _ := lensClient.NewChainClient(GetLensConfig(conf, true), conf.Homepath, nil, nil)
	return cl
}

func GetLensConfig(conf lens, debug bool) *lensClient.ChainClientConfig {
	return &lensClient.ChainClientConfig{
		Key:            conf.Key,
		ChainID:        conf.ChainID,
		RPCAddr:        conf.Rpc,
		GRPCAddr:       "UNSUPPORTED",
		AccountPrefix:  conf.AccountPrefix,
		KeyringBackend: conf.KeyringBackend,
		GasAdjustment:  1.2,
		GasPrices:      "0ustake",
		KeyDirectory:   conf.Homepath,
		Debug:          debug,
		Timeout:        "10s",
		OutputFormat:   "json",
		SignModeStr:    "direct",
		Modules:        lensClient.ModuleBasics,
	}
}
