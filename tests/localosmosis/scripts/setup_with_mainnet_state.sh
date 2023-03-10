#!/bin/sh
set -e 
set -o pipefail

OSMOSIS_HOME=$HOME/.osmosisd
CONFIG_FOLDER=$OSMOSIS_HOME/config

DEFAULT_MNEMONIC="only item always south dry begin barely seed wire praise chapter bomb remind abandon erase safe point vehicle tuition release half denial receive water"
DEFAULT_CHAIN_ID="localosmosis"
DEFAULT_MONIKER="localosmosis-validator"

# Override default values with environment variables
MNEMONIC=${MNEMONIC:-$DEFAULT_MNEMONIC}
CHAIN_ID=${CHAIN_ID:-$DEFAULT_CHAIN_ID}
MONIKER=${MONIKER:-$DEFAULT_MONIKER}

# State export url
STATE_EXPORT_URL=https://state-exports.osmosis.zone/latest.json

install_prerequisites () {
    apk add -q --no-cache \
        dasel \
        python3 \
        py3-pip
}

edit_config () {

    # Remove seeds
    dasel put string -f $CONFIG_FOLDER/config.toml '.p2p.seeds' ''

    # Disable fast_sync
    dasel put bool -f $CONFIG_FOLDER/config.toml '.fast_sync' 'false'

    # Expose the rpc
    dasel put string -f $CONFIG_FOLDER/config.toml '.rpc.laddr' "tcp://0.0.0.0:26657"
}

if [[ ! -d $CONFIG_FOLDER ]]
then
    install_prerequisites

    # Download state export
    echo "Downloading state export..."
    LATEST_STATE_EXPORT=$(wget -qO- $STATE_EXPORT_URL)
    wget -q $LATEST_STATE_EXPORT -O /osmosis/state_export.json

    echo $MNEMONIC | osmosisd init -o --chain-id=$CHAIN_ID --home $OSMOSIS_HOME --recover $MONIKER 2> /dev/null
    echo $MNEMONIC | osmosisd keys add localosmosis-validator --recover --keyring-backend test > /dev/null 2>&1

    ACCOUNT_PUBKEY=$(osmosisd keys show --keyring-backend test localosmosis-validator --pubkey | dasel -r json '.key' --plain)
    ACCOUNT_ADDRESS=$(osmosisd keys show -a --keyring-backend test localosmosis-validator --bech acc)

    VALIDATOR_PUBKEY_JSON=$(osmosisd tendermint show-validator --home $OSMOSIS_HOME)
    VALIDATOR_PUBKEY=$(echo $VALIDATOR_PUBKEY_JSON | dasel -r json '.key' --plain)
    VALIDATOR_HEX_ADDRESS=$(osmosisd debug pubkey $VALIDATOR_PUBKEY_JSON 2>&1 --home $OSMOSIS_HOME | grep Address | cut -d " " -f 2)
    VALIDATOR_ACCOUNT_ADDRESS=$(osmosisd debug addr $VALIDATOR_HEX_ADDRESS 2>&1  --home $OSMOSIS_HOME | grep Acc | cut -d " " -f 3)
    VALIDATOR_OPERATOR_ADDRESS=$(osmosisd debug addr $VALIDATOR_HEX_ADDRESS 2>&1  --home $OSMOSIS_HOME | grep Val | cut -d " " -f 3)
    VALIDATOR_CONSENSUS_ADDRESS=$(osmosisd debug bech32-convert $VALIDATOR_OPERATOR_ADDRESS -p osmovalcons  --home $OSMOSIS_HOME 2>&1)

    python3 -u testnetify.py \
    -i /osmosis/state_export.json \
    -o $CONFIG_FOLDER/genesis.json \
    -c $CHAIN_ID \
    --validator-hex-address $VALIDATOR_HEX_ADDRESS \
    --validator-operator-address $VALIDATOR_OPERATOR_ADDRESS \
    --validator-consensus-address $VALIDATOR_CONSENSUS_ADDRESS \
    --validator-pubkey $VALIDATOR_PUBKEY \
    --account-pubkey $ACCOUNT_PUBKEY \
    --account-address $ACCOUNT_ADDRESS \
    --prune-ibc

    edit_config
fi

osmosisd start --home $OSMOSIS_HOME --x-crisis-skip-assert-invariants