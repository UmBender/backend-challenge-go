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

## Filas

Criadas automaticamente pelo LocalStack na inicialização (`deploy/localstack/init/ready.d/10-queues.sh`):

- `wager-transactions.fifo` — entrada, com redrive para a DLQ após 5 recebimentos;
- `wager-transactions-dlq.fifo` — DLQ;
- `wallet-events.fifo` — eventos publicados pelo serviço.

## Migrations

`golang-migrate`, executado como `wallet_owner` (nunca como o role da aplicação). O Compose aplica as pendentes no `up`.

```sh
make migrate-up        # aplica as pendentes
make migrate-down      # reverte a última
make migrate-version   # versão atual
```

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

Integração (build tag `integration`), com a infraestrutura no ar e as variáveis exportadas no mesmo shell:

```sh
docker compose up -d postgres localstack migrate
set -a; . ./.env.example; set +a
make test-integration  # go test -tags=integration -race -count=1 ./...
```

Os testes de banco criam um banco isolado por teste com o superusuário de `TEST_ADMIN_DATABASE_URL`, aplicam as migrations como `wallet_owner` e testam como `wallet_app`. Nunca tocam o banco `wallet` das réplicas.

## Problemas comuns

- **`Permission denied` em `/docker-entrypoint-initdb.d` (SELinux):** os bind mounts usam `:z`; mantenha o sufixo.
- **Mudança no initdb sem efeito:** ele só roda num volume novo — `make down-volumes`.
- **`DATABASE_URL is not set` nos testes de integração:** faltou o `set -a; . ./.env.example; set +a`.
- **Porta ocupada:** ajuste `POSTGRES_HOST_PORT`, `LOCALSTACK_HOST_PORT` ou `KEYCLOAK_HOST_PORT` no `.env`.
