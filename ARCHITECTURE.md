# Arquitetura

Decisões técnicas do serviço de carteiras, conforme o §15 do [enunciado](PROPOSAL.md). Cada seção é atualizada quando a funcionalidade correspondente é implementada.

## Visão geral

Um único binário (`cmd/wallet`) roda a API HTTP e, nas próximas etapas, o consumidor SQS e os workers. Rodam três réplicas idênticas; a corretude depende do PostgreSQL, nunca de estado em memória.

```text
cmd/wallet/          ponto de entrada
cmd/healthcheck/     probe do HEALTHCHECK (imagem sem shell)
internal/bootstrap/  composição Fx de todos os módulos
internal/platform/   config, logging, métricas, health
internal/adapters/   postgres, sqsclient, httpapi
migrations/          SQL versionado (up/down)
deploy/              initdb do PostgreSQL, filas do LocalStack, realm do Keycloak
```

## Uso do Fx

- `bootstrap.Modules()` é a lista única de módulos, usada pelo `main` e pelos testes. `fx.ValidateApp` valida o grafo sem Docker.
- Cada pacote expõe um `fx.Module`. Os checks de readiness entram por value group (`health.AsCheck`), então um adapter novo registra o seu check sem alterar `health`.
- `config.Module` fornece as sub-structs (`config.HTTP`, `config.Database`...), para que cada construtor dependa só do que usa.
- A ordem de start e stop vem do grafo: o Fx executa `OnStart` na ordem de construção e `OnStop` ao contrário. O pool do PostgreSQL é construído antes do servidor HTTP, logo fecha depois dele, sem ordenação manual.
- Só `cmd/`, `bootstrap` e os arquivos de módulo importam Fx; o domínio não importará Fx, pgx, net/http nem AWS.

## Ciclo de vida e shutdown

- **Start:** o `OnStart` faz `Ping` no PostgreSQL e resolve as três filas; falha em qualquer um aborta o processo (fail fast). O servidor faz `net.Listen` síncrono, então porta ocupada também falha o start. Se o `Serve` morrer depois, a aplicação inteira encerra com código 1.
- **Stop (SIGTERM):** readiness passa a 503 → o HTTP drena as requisições em andamento → o pool do PostgreSQL fecha → exit 0.
- **Orçamentos encadeados:** `HTTP_SHUTDOWN_TIMEOUT` (10 s) < `SHUTDOWN_TIMEOUT` (25 s, stop do Fx) < `stop_grace_period` (30 s, Compose). A config rejeita valores fora dessa ordem. Os drenos do consumidor e dos workers terão de caber nos mesmos 25 s.

## Configuração

Variáveis de ambiente lidas com a stdlib e validadas no start; todos os erros são reportados de uma vez. Credenciais AWS ficam com a cadeia padrão do SDK, fora das structs da aplicação. `DATABASE_URL` contém senha e nunca é logada nem incluída em mensagens de erro.

## Health checks e observabilidade

- `GET /health/live` não consulta dependências: uma oscilação do banco não deve reiniciar todas as réplicas.
- `GET /health/ready` executa os checks de PostgreSQL e SQS em paralelo, com 1 s de timeout cada. A resposta diz apenas `ok`/`unavailable`; o erro detalhado vai para o log.
- Logs JSON (`log/slog`), sempre com `service` e `instanceId`. Os eventos internos do Fx saem no mesmo formato.
- Métricas Prometheus em `/metrics`, com registry dedicado. `http_request_duration_seconds` usa como label o padrão da rota (`GET /wallets/{walletId}`), nunca o caminho bruto, para limitar a cardinalidade.

## PostgreSQL e migrations

- Driver `pgx/v5` com `pgxpool`; até 10 conexões por réplica.
- Dois roles, criados pelo initdb na primeira inicialização do volume:
  - `wallet_owner` é dono do banco e roda as migrations; não é superusuário, então triggers também o restringem;
  - `wallet_app` é o role da aplicação e só recebe os grants que cada migration concede explicitamente.
- `000001` revoga os privilégios de `PUBLIC` no schema e concede apenas `USAGE` a `wallet_app`.
- `golang-migrate`, com `.up.sql` e `.down.sql` separados, roda como job único no Compose antes das réplicas.

## Mensageria

LocalStack com três filas FIFO, criadas na inicialização. A fila de entrada redireciona para a DLQ após 5 recebimentos, não usa deduplicação por conteúdo (os produtores enviam ids explícitos) e faz long polling de 20 s. Valores a confirmar junto com o consumidor.

## Autenticação

Keycloak sobe com o realm `wagering` importado, ainda sem clients. A integração (clients, validação de JWT, autorização por provedor) ainda não foi implementada.

## Imagem e Compose

- Imagem multi-stage, binários estáticos, runtime distroless sem root.
- Portas publicadas apenas em `127.0.0.1`. Bind mounts com `:z` para SELinux.
- As réplicas sobem só depois de PostgreSQL e LocalStack saudáveis e das migrations concluídas.

## Testes

- `go test ./...`: config, logging, health, router, ciclo de vida do servidor HTTP e validação do grafo Fx, sem Docker.
- `make test-integration` (build tag `integration`): a aplicação completa contra PostgreSQL e SQS reais — start, readiness verde, stop com pool fechado e porta liberada, e falha no start quando uma fila não existe.

## Limitações e trabalho não concluído

- Ainda não implementados: dinheiro, domínio, schema de negócio, idempotência, locks, referências pendentes, reversões, inbox/outbox, autenticação e autorização.
- O `iss` dos tokens difere entre o host (`localhost:8180`) e a rede Docker (`keycloak:8080`); precisa ser resolvido na integração com o IdP.
- `/metrics` é público na porta da API.
- Readiness inclui o SQS, como pede o enunciado: com o SQS fora, a réplica fica *not ready* mesmo podendo atender HTTP.
- Sem `restart:` nas réplicas: uma falha transitória no boot deixa a réplica parada.
- Imagens fixadas por tag, não por digest.
