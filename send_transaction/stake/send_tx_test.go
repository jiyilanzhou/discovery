package main

import (
	"crypto/ecdsa"
	"encoding/hex"
	"testing"
	"truechain/discovery/common"
	"truechain/discovery/crypto"
)

func TestWriteJson(t *testing.T) {
	delegateNum = 6
	kas := make(KeyAccount, delegateNum)
	delegateKey = make([]*ecdsa.PrivateKey, delegateNum)
	delegateAddr = make([]common.Address, delegateNum)
	for i := 0; i < delegateNum; i++ {
		delegateKey[i], _ = crypto.GenerateKey()
		delegateAddr[i] = crypto.PubkeyToAddress(delegateKey[i].PublicKey)
		kas[delegateAddr[i]] = hex.EncodeToString(crypto.FromECDSA(delegateKey[i]))
	}
	writeNodesJSON(defaultKeyAccount, kas)
}
