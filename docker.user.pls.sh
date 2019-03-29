#!/usr/bin/env bash
DATADIR=$HOME/.pls.dev

rm -rf $DATADIR

function get_rootchain() {
  ROOTCHAIN=`curl -X POST --data '{"jsonrpc":"2.0","method":"eth_rootChain","params":[],"id":67}' -H "Content-Type: application/json" ${OPERATOR_ADDR}:8547 --silent |  sed -n 's/.*\(0x.*\)\".*/\1/p'`
}

# First Try to get RootChain address from childchain.
get_rootchain

# condition check
# address_count=$(echo -n $ROOTCHAIN | wc -m)
while [ $(echo ${#ROOTCHAIN}) != 42 ];
do
  echo "Waiting for deploy rootchain contract"
  sleep 1s
  get_rootchain
done
echo "Got RootChain address from Child Chain : " $ROOTCHAIN

geth \
  --datadir $DATADIR \
  --rpc \
  --networkid 1337 \
  --rpcaddr 0.0.0.0 \
  --rpcport 8547 \
  --port 30307 \
  --rootchain.url $ROOTCHAIN_ADDR:8546 \
  --rootchain.contract $ROOTCHAIN \
  --syncmode "full" \
  --bootnodes "enode://401bd6383fe11a5224d5b4277b53d7c0278efed3ca685b6593935751ad1fe734a8e35d2b3ebd9d7fc6da6cff12e72cfcfca8db408bf2e49f1fad4c503956d07f@${BOOTNODE_ADDR}:30301" > log/user_node.log
