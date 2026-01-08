# [faucet.coinbin.org](https://faucet.coinbin.org/)

try now at [faucet.coinbin.org](https://faucet.coinbin.org/)

<img width="838" height="587" alt="image" src="https://github.com/user-attachments/assets/ab4cdf43-1d44-4912-adaa-7f3bf758fe19" />



## features

- supports signet, maybe works for testnet too
- rich admin interface and many configurations options, many optional
- set withdraw limits
- auto-consolidations jobs
- prometheus metrics


### quick run with docker

```
# website on  :7766
# metrics on  :9844


docker run  \
    -d --name faucet \
    --restart=always \
    -v /data/signet/faucet/data:/data  \
    -p 7766:7766 \
    -p 9844:9844  \
    lnliz/faucet.coinbin.org:latest   \
    -bitcoin-rpc-host <<rpc-host>> \
    -bitcoin-rpc-user <<rpc-user>> \
    -bitcoin-rpc-password <<rpc-pass>> \
    -min-amount=0.75 \
    -max-amount=1.50 \
    -admin-path=/admin  \
    -admin-2fa-secret="..." \
    -admin-password admin-password \
    -admin-ip=123.123.123.123 \
    -admin-cookie-secret="..."   \
    -turnstile-site-key="0x4AAAAAA..." \
    -turnstile-secret="0x4AAAAAA..." \
    -data-dir /data \
    -batch-interval=10s \
    -consolidation-amount-threshold=2.1 \
    -consolidation-min-utxos=10 \
    -consolidation-max-utxos=35 \
    -auto-consolidation-interval=9m \
    -max-withdrawals-per-ip-24h=4 \
    -metrics-addr=0.0.0.0:9844 \
    -listen 0.0.0.0:7766
```
