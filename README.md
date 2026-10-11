# Desafio Backend — Processamento Distribuído de Apostas em Go

Serviço de carteiras para operações de apostas. Enunciado: [PROPOSAL.md](PROPOSAL.md). Decisões técnicas: [ARCHITECTURE.md](ARCHITECTURE.md).

## Pré-requisitos

- Docker com Docker Compose v2. No Fedora: `sudo dnf install moby-engine docker-compose && sudo systemctl enable --now docker`.
- Go 1.26.8, para rodar os testes no host.
- `make` (opcional; `make help` lista os alvos).

## Subir o ambiente

```sh
cp .env.example .env      # opcional: o Compose já tem valores locais padrão
docker compose up --build
```

Sobem PostgreSQL, LocalStack (SQS), Keycloak, o job de migrations e três réplicas do serviço:

| Serviço | Endereço no host |
| --- | --- |
| wallet-1 / wallet-2 / wallet-3 | http://127.0.0.1:8081 · :8082 · :8083 |
| PostgreSQL | 127.0.0.1:5432 (banco `wallet`) |
| LocalStack (SQS) | http://127.0.0.1:4566 |
| Keycloak (admin/admin) | http://127.0.0.1:8180 |

```sh
docker compose ps                     # réplicas "healthy"
curl -s localhost:8081/health/ready   # {"ready":true,"checks":{"postgres":"ok","sqs":"ok"}}
curl -s localhost:8081/health/live
curl -s localhost:8081/metrics | head
```

## Endpoints

| Método e rota | Quem pode chamar |
| --- | --- |
| `POST /wagering/transactions` (header `Idempotency-Key` obrigatório) | provedor |
| `GET /wagering/transactions/{transactionId}` | provedor dono da transação, serviço interno |
| `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` | o próprio provedor, serviço interno |
| `POST /wallets` | serviço interno |
| `GET /wallets/{walletId}` | serviço interno |
| `GET /wallets/{walletId}/ledger?limit=&cursor=` | serviço interno |
| `POST /wallets/{walletId}/reconciliation` | serviço interno |
| `GET /health/live`, `GET /health/ready`, `GET /metrics` | público |

Todas as rotas de negócio exigem um token Bearer emitido pelo Keycloak (`client_credentials`). O realm `wagering` é importado automaticamente com estas identidades de teste (segredos locais, não reais):

| Client | Segredo | Papéis | `provider_id` |
| --- | --- | --- | --- |
| `provider-a` | `provider-a-secret` | `wager:submit`, `wager:read` | `provider-a` |
| `provider-b` | `provider-b-secret` | `wager:submit`, `wager:read` | `provider-b` |
| `wallet-internal` | `wallet-internal-secret` | `wallet:admin` | — |
| `provider-a-short-lived` | `provider-a-short-lived-secret` | como `provider-a`, token de 1 s (testes de expiração) | `provider-a` |
| `other-service` | `other-service-secret` | sem a audience `wallet-api` (testes de rejeição) | — |

Fluxo autenticado (usa `jq`):

```sh
token() { curl -s -d grant_type=client_credentials -d client_id=$1 -d client_secret=$1-secret \
  http://localhost:8180/realms/wagering/protocol/openid-connect/token | jq -r .access_token; }

curl -s -X POST localhost:8081/wallets -H "Authorization: Bearer $(token wallet-internal)" \
  -H 'Content-Type: application/json' \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"100.00","currency":"BRL"}}'
# {"id":"<walletId>",...,"balance":{"amount":"100.00","currency":"BRL"},"version":1}

curl -s -X POST localhost:8082/wagering/transactions -H "Authorization: Bearer $(token provider-a)" \
  -H 'Idempotency-Key: provider-a:bet-1' -H 'Content-Type: application/json' \
  -d '{"providerId":"provider-a","externalTransactionId":"bet-1","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"<walletId>","roundId":"r1","gameId":"g1","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
# 201 {"transactionId":"...","status":"PROCESSED","balance":{"amount":"75.00","currency":"BRL"},"idempotentReplay":false}
# Repetir o mesmo comando (em qualquer réplica) devolve 200 com "idempotentReplay":true e o mesmo saldo.
```

Sem token ou com token inválido/expirado: 401. Provedor chamando `/wallets*`: 403.

Os fluxos completos são exercitados pelos testes de integração `internal/adapters/httpapi/api_integration_test.go` e, com tokens reais do Keycloak, `keycloak_integration_test.go`.

## Filas

Criadas automaticamente pelo LocalStack na inicialização (`deploy/localstack/init/ready.d/10-queues.sh`):

- `wager-transactions.fifo` — entrada, com redrive para a DLQ após 5 recebimentos;
- `wager-transactions-dlq.fifo` — DLQ;
- `wallet-events.fifo` — eventos publicados pelo serviço.

Enviar uma operação pela fila (o mesmo caso de uso do HTTP; `MessageGroupId` = carteira, `MessageDeduplicationId` = `messageId`):

```sh
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id <walletId> --message-deduplication-id msg-123 \
  --message-body '{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123","playerId":"<playerId>","walletId":"<walletId>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}'
```

Mensagens inválidas ou com erro permanente vão para `wager-transactions-dlq.fifo`; falhas transitórias são repetidas e, após 5 recebimentos, também vão para a DLQ.

Os eventos são publicados pelo outbox de cada réplica. Para inspecioná-los:

```sh
docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/000000000000/wallet-events.fifo --max-number-of-messages 10
```

## Migrations

`golang-migrate`, executado como `wallet_owner` (nunca como o role da aplicação). O Compose aplica as pendentes no `up`.

```sh
make migrate-up        # aplica as pendentes
make migrate-down      # reverte a última
make migrate-version   # versão atual
```

| Migration | Conteúdo |
| --- | --- |
| `000001_schema_privileges` | privilégios do schema `public` |
| `000002_wallets` | carteiras e trigger de versão |
| `000003_wager_transactions` | transações, idempotência por provedor, estado terminal imutável |
| `000004_ledger` | ledger append-only |
| `000005_inbox_outbox` | inbox e outbox |

Reverter tudo: `docker compose run --rm migrate -path=/migrations -database=postgres://wallet_owner:wallet_owner@postgres:5432/wallet?sslmode=disable down -all`. Reverter a `000004` apaga o ledger.

Os roles são criados por `deploy/postgres/initdb/10-roles.sh` apenas na primeira inicialização do volume. Para recriar do zero: `make down-volumes`.

## Variáveis de ambiente

Todas estão em [.env.example](.env.example), com valores locais. Obrigatórias: `DATABASE_URL` e `AWS_REGION`. Credenciais AWS seguem a cadeia padrão do SDK. Configuração inválida encerra o processo com código 1, listando todos os problemas.

## Testes

```sh
go test ./...
go test -race ./...
go vet ./...
make check             # gofmt + vet + testes com -race
```

Integração (build tag `integration`), com a infraestrutura no ar:

```sh
docker compose up -d postgres localstack keycloak migrate
make test-integration  # variáveis não definidas no ambiente vêm do .env.example
```

Os testes de banco criam um banco isolado por teste com o superusuário de `TEST_ADMIN_DATABASE_URL`, aplicam as migrations como `wallet_owner` e testam como `wallet_app`. Nunca tocam o banco `wallet` das réplicas.

Os testes de integração usam PostgreSQL, LocalStack e Keycloak reais: cada teste de banco cria seu próprio banco, e os testes de SQS criam filas próprias, removidas no fim.

### Múltiplas instâncias e simulação de falhas

Com a stack completa no ar (`docker compose up --build -d` e as três réplicas `healthy`):

```sh
make test-e2e   # go test -tags=e2e ./tests/e2e/ contra wallet-1, wallet-2 e wallet-3
```

Os cenários são caixa-preta, pelos três processos separados, com tokens reais do Keycloak:

- duas apostas de 80 sobre 100 em réplicas diferentes: uma processada, uma `INSUFFICIENT_FUNDS`, saldo 20.00;
- a mesma aposta 30 vezes em paralelo, distribuída entre as réplicas: um débito só;
- dez carteiras com dez apostas cada, por todas as réplicas;
- `REFUND` antes do `BET` em réplicas diferentes, concluído pelo worker de qualquer uma;
- a mesma operação por HTTP e por SQS: um efeito só;
- **falha:** `wallet-2` é morta com `SIGKILL` (`docker compose kill -s SIGKILL wallet-2`) no meio da carga; os clientes repetem com a mesma `Idempotency-Key` em outra réplica, cada aposta é aplicada uma única vez, e a réplica é religada no fim;
- no fim de cada cenário, a reconciliação confirma saldo = ledger; por último, o outbox de todas as réplicas fica vazio.

O cenário de falha usa o `docker` CLI e religa `wallet-2` sozinho. Para repetir manualmente: `docker compose kill -s SIGKILL wallet-2`, depois `docker compose start wallet-2`; para parada graciosa, `docker compose stop wallet-2` (logs mostram a drenagem e exit 0).

## Problemas comuns

- **`Permission denied` em `/docker-entrypoint-initdb.d` (SELinux):** os bind mounts usam `:z`; mantenha o sufixo.
- **Mudança no initdb sem efeito:** ele só roda num volume novo — `make down-volumes`.
- **`... is not set` ao rodar `go test -tags=integration` direto:** use `make test-integration`, ou exporte as variáveis com `set -a; . ./.env.example; set +a`. Valores exportados antigos têm prioridade; se o seu `.env` é de uma versão anterior, copie de novo o `.env.example`.
- **Porta ocupada:** ajuste `POSTGRES_HOST_PORT`, `LOCALSTACK_HOST_PORT` ou `KEYCLOAK_HOST_PORT` no `.env`.
