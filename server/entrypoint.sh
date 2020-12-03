#!/usr/bin/env bash


CONF_PATH=$HOME/.dcrdex

if [ ! -d $CONF_PATH ]; then
    mkdir -p $CONF_PATH
fi    

if [ ! -f $CONF_PATH/dexprivkey ]; then
    cd $CONF_PATH && \
    genkey
fi

cd $HOME

if [ ! -f $CONF_PATH/markets.json ]; then
    cp sample-markets.json $CONF_PATH/markets.json
fi



dcrdex

