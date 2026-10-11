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

## Dinheiro

- `Money{minor int64; currency Currency}` em `internal/domain/money`: imutável, campos não exportados, construído só por `Parse`, `FromMinor` e `Zero`. No transporte é a string `"25.00"`; no banco será `BIGINT` em centavos.
- Nenhum ponto flutuante em parsing, aritmética, formatação ou JSON. `internal/archtest` percorre a AST dos caminhos de dinheiro e falha em `float32/float64`, `strconv.*Float*`, `math`, `math/big`, `json.Number`, `.Float64()` e verbos `%f/%e/%g`; um fixture prova que cada regra dispara.
- Gramática de entrada estrita, `^(0|[1-9][0-9]*)\.[0-9]{2}$`: exatamente duas casas, sem sinal, sem zeros à esquerda. `"25"`, `"25.0"` e `"01.00"` são rejeitados, nunca normalizados nem arredondados. Cada valor tem uma única forma textual, então o hash de idempotência não precisa de normalização. Fuzz garante que `Parse(s).String() == s`.
- Overflow do `int64` é verificado em `Parse`, `Add`, `Sub` e `Neg` (inclusive `-MinInt64`).
- `Currency` é opaca, com whitelist `BRL` e `USD` (esta para os testes de divergência). Toda moeda suportada tem duas casas decimais; uma moeda com outra escala exigiria escala por moeda.
- O zero value `Money{}` é rejeitado por toda operação; moedas diferentes em `Add`, `Sub` e `Cmp` são erro.
- JSON `{"amount":"25.00","currency":"BRL"}`. A leitura rejeita números JSON, chaves desconhecidas, duplicadas ou com outra caixa, e aplica `Parse` (valores negativos não entram por fronteira externa). Valores negativos internos, como diferenças, podem ser serializados.

## Domínio e máquina de estados

- Pacotes `internal/domain/{money,ledger,wallet,event,wager}`. O domínio só importa a stdlib sem I/O, `google/uuid` e o próprio domínio; `internal/archtest` garante isso. Nenhum `panic` para resultados de negócio.
- `Wallet`: `New` cria na versão 1 com o saldo inicial; `Rehydrate` só valida o que veio do banco. `Debit`/`Credit` devolvem o `Movement`, incrementam a versão apenas quando o saldo muda e não alteram nada em caso de recusa.
- `ledger.NewEntry` exige `after = before ± amount`, valor positivo, mesma moeda, saldos não negativos e versão ≥ 1.
- `wager.Transaction`: `NewExternal` nasce `PENDING`; `NewOpening` registra a abertura positiva (crédito de 0 ao saldo inicial na versão 1, com os dois eventos); `Rehydrate` reaplica as regras do schema.
- Transições: `PENDING` e `PENDING_REFERENCE` → `PENDING_REFERENCE`, `PROCESSED`, `REJECTED` ou `FAILED`; estados terminais não aceitam nada. A matriz 5 × 5 é testada.
- `Apply` devolve uma `Decision` (processada, rejeitada ou aguardando referência). Rejeições de negócio são decisões persistidas e repetidas em replays, não erros. Ordem: carteira, jogador, moeda, referência, saldo.

| Tipo | Efeito | Referência |
| --- | --- | --- |
| `BET` | débito; sem saldo → `INSUFFICIENT_FUNDS` | proibida (400) |
| `WIN` | crédito | opcional; se informada, `BET` processado da mesma rodada |
| `LOSS` | nenhum (sem ledger, versão igual) | proibida (400) |
| `REFUND` | crédito do valor do `BET` | obrigatória, `BET`, mesmo valor |
| `ROLLBACK` | inverso do original | obrigatória, `BET`/`WIN`/`REFUND`, mesmo valor; débito sem saldo → `REVERSAL_INSUFFICIENT_FUNDS` |

## Referências pendentes e reversões

- Referência ausente ou ainda não terminal → `PENDING_REFERENCE`. O evento `WagerTransactionPendingReference` sai só na primeira espera. A espera expira por TTL ou número máximo de tentativas, com `REFERENCE_NOT_FOUND` (ou `REFERENCE_NOT_PROCESSED`, se a referência existe mas nunca terminou). O próximo horário (backoff com jitter) é calculado pela aplicação.
- Referência `REJECTED`/`FAILED` → `REFERENCE_NOT_PROCESSED`; atributos divergentes (provedor, jogador, carteira, moeda, rodada) → `REFERENCE_MISMATCH`; tipo errado → `REFERENCE_KIND_INVALID`; valor diferente → `REFERENCE_AMOUNT_MISMATCH`.
- No máximo um estorno bem-sucedido por transação, de qualquer tipo; uma segunda tentativa recebe `ALREADY_REVERSED`. Não há estorno parcial.

### Worker de referências pendentes

- Toda réplica roda um `PendingWorker`. Ele reivindica uma linha `PENDING_REFERENCE` vencida por vez com `FOR NO KEY UPDATE SKIP LOCKED`, trava a carteira, relê a referência e aplica a mesma decisão do domínio, persistindo pelo mesmo caminho do `SubmitWager`, tudo numa transação. Dois workers nunca processam a mesma linha, e a ordem de locks é a do caminho síncrono (linha da transação, depois carteira).
- O estado da espera (`attempts`, `next_attempt_at`, `expires_at`) está na linha, não em memória: qualquer réplica retoma o trabalho de outra, inclusive de uma que morreu.
- Sem referência: reagenda com backoff exponencial e jitter (`PENDING_BACKOFF_BASE` a `PENDING_BACKOFF_CAP`), sem novo evento. Expirado o TTL ou o número máximo de tentativas: `REJECTED` com `REFERENCE_NOT_FOUND` e o evento de rejeição.
- Uma falha permanente que não é de negócio grava `FAILED` (`PROCESSING_FAILED`) em outra transação, para não reprocessar a linha para sempre; falhas transitórias apenas tentam de novo.
- A espera é por polling (1 s quando não há trabalho), sem acordar o estorno quando a referência chega: a latência extra é de no máximo um intervalo de backoff.
- Testes de integração: `REFUND` antes do `BET` processado pelo worker (saldo, eventos, reconciliação), reagendamento e expiração por TTL, e dois workers concorrentes sobre dez carteiras sem processamento duplicado.

## Eventos

Um tipo Go por evento, com interface selada; tipo e versão vêm do payload. O `aggregateId` de todos é a carteira, o que dá ordem por carteira na fila FIFO. O envelope é imutável, e `MarshalJSON` falha com `Money` inválido, então um evento quebrado nunca é gravado. `eventId`, correlação, causa e `occurredAt` (UTC) vêm da aplicação.

## Schema e imutabilidade

Cada invariante financeira tem uma constraint, um índice único parcial ou um trigger no banco, além da regra no domínio. Os testes de integração verificam cada uma pelo SQLSTATE.

- `wallets`: `balance_minor >= 0`, `version >= 1`, `UNIQUE (player_id, currency)`. Um trigger exige versão + 1 se e somente se o saldo muda, e congela a identidade.
- `wager_transactions`: forma interna × externa (`OPENING` sem metadados de provedor; externas com chave, hash SHA-256, rodada e jogo), política de zero, referência obrigatória em estornos e proibida em `BET`/`LOSS`/`OPENING`, código de falha apenas em `REJECTED`/`FAILED`. Índices únicos parciais: `(provider_id, external_transaction_id)`, `(provider_id, idempotency_key)`, uma `OPENING` por carteira e um estorno `PROCESSED` por referência. Um trigger impede mudanças em estados terminais, volta a `PENDING` e alteração dos campos de identidade e do hash.
- `wallet_ledger_entries`: aritmética conferida por linha, valor positivo, `UNIQUE (wallet_id, transaction_id)` e `UNIQUE (wallet_id, wallet_version)`, que transforma uma atualização perdida em violação de unicidade. FKs compostas garantem a moeda da carteira e uma transação da mesma carteira; `seq` serve à paginação.
- Ledger append-only em duas camadas: `wallet_app` só tem `SELECT, INSERT`, e triggers bloqueiam `UPDATE`/`DELETE`/`TRUNCATE` para todos exceto superusuários, inclusive o dono que roda as migrations.
- `inbox_messages`: PK `(consumer_name, message_id)` e hash do payload. `outbox_events`: o envelope é imutável; só os campos de entrega mudam, e uma linha publicada é final.
- Grants explícitos por tabela: `wallet_app` tem `SELECT, INSERT, UPDATE` nas demais tabelas e nenhum `DELETE`/`TRUNCATE`; não há `ALTER DEFAULT PRIVILEGES`.
- Triggers de invariante usam `check_violation` (23514); o do ledger usa `restrict_violation` (23001), para separar "proibido por desenho" de uma checagem que falhou.
- Os testes de schema criam um banco por teste, aplicam as migrations como `wallet_owner` e executam como `wallet_app`, provando que os privilégios, e não só os triggers, protegem o ledger.

## Transações, locks e concorrência

- `app.TxManager` é o único dono de `BEGIN`/`COMMIT`/`ROLLBACK`; os repositórios recebem a transação corrente. Um caso de uso corresponde a uma transação SQL.
- `SubmitWager`, em READ COMMITTED:
  1. `INSERT … ON CONFLICT DO NOTHING`: o índice único decide duplicatas, e uma duplicata concorrente espera a primeira commitar;
  2. `SELECT … FOR NO KEY UPDATE` na carteira: serializa as escritas da mesma carteira sem conflitar com o `FOR KEY SHARE` que a FK do passo 1 já tomou (com `FOR UPDATE`, duas operações na mesma carteira entrariam em deadlock);
  3. a referência e o "já estornada?" são lidos depois do lock;
  4. o domínio decide;
  5. `UPDATE wallets … WHERE version = $lida`, ledger, transação e outbox; `COMMIT`.
- Estratégia pessimista por carteira, com as constraints como barreira final (`UNIQUE (wallet_id, wallet_version)`, saldo ≥ 0, estorno único). A ordem dos locks é fixa (entrada no índice de idempotência, depois uma carteira), então não há deadlock; não há lock global, e carteiras diferentes não se bloqueiam.
- Em READ COMMITTED, a segunda de duas apostas concorrentes acorda já vendo o saldo novo e é rejeitada sem retry. REPEATABLE READ ou SERIALIZABLE a abortariam com `40001`.
- Nada é commitado como `PENDING` no caminho síncrono. Os retries ficam numa camada só: `ErrConcurrentConflict` repete a transação inteira até 3 vezes, com jitter.
- Sessões com `lock_timeout = 5s` e `statement_timeout = 15s`; estourar vira `ErrUnavailable` (503).
- Erros do PostgreSQL são classificados num único lugar: conflito de concorrência (`40001`, `40P01`, `23505` não mapeado) → retry; indisponibilidade (`55P03`, `57014`, `57P0x`, `53300`, classe `08`, rede) → 503; violação de invariante (`23514`, `23503`, `23502`, `23001`, `42501`) → 500. O erro original continua na cadeia para os logs.

## Idempotência

- Escopo por provedor: `(provider_id, idempotency_key)` e `(provider_id, external_transaction_id)` são únicos no banco, nunca em memória.
- Mesma chave e mesmo hash → replay do resultado armazenado, com o saldo da época; mesma chave e hash diferente → `IDEMPOTENCY_KEY_REUSED`; mesmo external id com outra chave → `EXTERNAL_TRANSACTION_CONFLICT`. Nos conflitos nada se move.
- Hash: SHA-256 do JSON canônico (chaves ordenadas, compacto, sem escape HTML) de `providerId`, `externalTransactionId`, `playerId`, `walletId`, `roundId`, `gameId`, `kind`, `money` e `referenceExternalTransactionId` quando presente. A chave e os metadados de transporte ficam de fora, então HTTP e SQS geram o mesmo hash. Não há normalização: a gramática do dinheiro já admite uma única forma por valor.

## Consultas e reconciliação

- Leituras em REPEATABLE READ READ ONLY. O ledger é paginado por keyset em `seq` (1 a 200 itens por página), estável enquanto novos lançamentos chegam.
- A reconciliação lê o saldo e a soma do ledger no mesmo snapshot. Uma divergência aparece na resposta, num log `WARN` e na métrica `wallet_reconciliation_divergence_total`; a reconciliação nunca escreve, para preservar a evidência.
- A política de referências pendentes é configurável: `PENDING_REFERENCE_TTL` (15m), `PENDING_MAX_ATTEMPTS` (10), `PENDING_BACKOFF_BASE` (1s), `PENDING_BACKOFF_CAP` (2m), com backoff exponencial e *equal jitter*.

## Contrato HTTP

- Roteamento com o `ServeMux` da stdlib. Cada rota de negócio é envolvida individualmente (correlation id, access log, timeout, autorização), então o padrão casado chega às métricas e ao log e as rotas públicas continuam públicas por construção.
- Corpo JSON lido de forma estrita: um único objeto, nomes exatos, sem duplicatas nem campos desconhecidos, limite de tamanho, `Content-Type: application/json`, UUIDs só na forma canônica e dinheiro como string. `Idempotency-Key` é obrigatório no envio de operações.
- O `providerId` vem do token; um corpo com outro provedor recebe 403 `PROVIDER_MISMATCH`. A transação de outro provedor é respondida com 404, para não confirmar que o id existe.
- Erros seguem a RFC 9457 (`application/problem+json`), com o membro `code` como contrato estável e o `correlationId`. Um único ponto (`writeError`) converte erros em respostas:

| Situação | Status | `code` / `failureCode` |
| --- | --- | --- |
| operação processada / replay | 201 / 200 (`idempotentReplay: true`, saldo da época) | — |
| aguardando referência | 202 + `Location` | — |
| rejeição de negócio (inclusive em replay) | 422 | `failureCode` do domínio |
| falha permanente registrada pelo worker | 500 | `PROCESSING_FAILED` |
| corpo, campo ou dinheiro inválido; `OPENING` vindo de fora | 400 | `INVALID_REQUEST`, `INVALID_MONEY`, `UNSUPPORTED_KIND`... |
| `Content-Type` errado / corpo grande demais | 415 / 413 | `UNSUPPORTED_MEDIA_TYPE` / `PAYLOAD_TOO_LARGE` |
| sem token ou token inválido | 401 | `UNAUTHENTICATED` |
| papel ausente / provedor divergente | 403 | `FORBIDDEN` / `PROVIDER_MISMATCH` |
| carteira ou transação inexistente | 404 | `WALLET_NOT_FOUND` / `TRANSACTION_NOT_FOUND` |
| chave reutilizada / external id com outra chave / carteira duplicada | 409 | `IDEMPOTENCY_KEY_REUSED` / `EXTERNAL_TRANSACTION_CONFLICT` / `WALLET_ALREADY_EXISTS` |
| indisponibilidade transitória, conflito esgotado, timeout | 503 + `Retry-After` | `SERVICE_UNAVAILABLE` |
| bug ou violação de invariante | 500 | `INTERNAL_ERROR` (detalhe só no log) |

- Cada requisição de negócio tem `HTTP_REQUEST_TIMEOUT` (10 s, menor que o write timeout); o prazo chega ao banco pelo context.
- O access log registra método, rota, status, duração, correlation id, request id, client e provedor, nunca corpos ou headers.
- O ledger é paginado com cursor opaco (`base64url` de JSON com o último `seq`), estável enquanto novos lançamentos chegam.

## Outbox e publicação de eventos

- Os eventos são gravados em `outbox_events` na mesma transação da mudança que os causou. Um evento existe se e somente se a mudança foi commitada, então nunca é publicado antes do commit nem perdido depois dele.
- Toda réplica roda um `OutboxRelay`. A cada ciclo ele reivindica até 50 eventos devidos numa única instrução: `SELECT … FOR UPDATE SKIP LOCKED` dentro de um `UPDATE` que grava o lease (`locked_by`, `locked_until` = 30 s). Publicadores concorrentes pegam lotes disjuntos, e nenhum lock fica aberto enquanto o evento viaja até o broker.
- Publicação em `wallet-events.fifo` com `MessageGroupId` = carteira (ordem por carteira) e `MessageDeduplicationId` = `eventId`. O payload é o envelope gravado; por ser `jsonb`, os espaços e a ordem das chaves podem diferir, mas o conteúdo é o mesmo.
- Sucesso: `published_at`, só se a linha ainda pertence ao lease do publicador. Falha: `attempts + 1`, `last_error`, próximo horário com backoff exponencial e jitter (1 s a 1 min) e lease liberado. O evento nunca é descartado; o relay tenta até o broker voltar.
- Entrega pelo menos uma vez. Se o processo morrer entre o envio e a marcação, o lease expira e outra réplica reenvia o mesmo `eventId`; a deduplicação do SQS absorve reenvios dentro da janela, e os consumidores devem deduplicar por `eventId`. A ordem por carteira é a ordem de `occurred_at` no caminho normal; após uma falha, um evento pode chegar depois de um mais novo, e `walletVersion` permite detectar isso.
- O dono do lease é `INSTANCE_ID` mais um sufixo aleatório por processo, para que uma réplica reiniciada não feche linhas reivindicadas pela encarnação anterior. No shutdown o loop é cancelado antes do pool fechar.
- Métricas: `outbox_events_published_total`, `outbox_publish_failures_total` e `outbox_lag_seconds` (idade do evento mais antigo não publicado).
- Testes de integração com PostgreSQL e LocalStack reais: publicação dos eventos commitados (grupo e deduplicação corretos), dois publicadores concorrentes enviando cada evento exatamente uma vez, broker fora do ar (nada perdido, reagendado e publicado na volta) e lease expirado retomado por outro publicador.

## Consumidor SQS e inbox

- Toda réplica consome `wager-transactions.fifo` com long polling de 20 s e até 10 mensagens por vez. Mensagens do mesmo `MessageGroupId` são processadas em ordem; grupos diferentes, em paralelo (até 4).
- O envelope do §10 é lido de forma estrita (campos desconhecidos rejeitados, UUIDs canônicos, dinheiro como string) e vira o mesmo `SubmitCommand` do HTTP, com a chave `data.idempotencyKey`: validação, hash e garantias financeiras são as do caminho HTTP.
- A inbox `(consumer_name, message_id)` é gravada e concluída na mesma transação SQL das mudanças de domínio, do ledger e dos eventos. Uma reentrega encontra a linha concluída e responde com o resultado armazenado, sem novo efeito; o mesmo `messageId` com outro conteúdo (hash do payload e da chave) é permanente e vai para a DLQ. Uma operação enviada por HTTP e depois por SQS é um replay. Uma referência pendente conclui a mensagem assim que a pendência é persistida; o worker continua a partir daí.
- A mensagem só é removida depois do commit. Se o processo morrer entre o commit e a remoção, a reentrega é um replay.
- Classificação:
  - sucesso, replay ou rejeição de negócio → remove;
  - mensagem malformada, request inválido, chave reutilizada, external id com outra chave, carteira inexistente, `messageId` reutilizado ou violação de invariante → encaminha explicitamente para `wager-transactions-dlq.fifo` (com o motivo em `failureReason`) e remove;
  - indisponibilidade, conflito esgotado, timeout ou erro desconhecido → torna a mensagem visível de novo com backoff (2 s a 30 s), sem processar as seguintes do mesmo grupo; após `maxReceiveCount = 5` recebimentos a política de redrive leva a mensagem para a DLQ.
- Limites: visibility timeout de 60 s na fila e 10 s por mensagem no processamento, então uma mensagem nunca é reentregue enquanto ainda está sendo tratada.
- `SIGTERM`: para de buscar mensagens, conclui as que estão em andamento (sem cancelá-las) e libera a visibilidade das que ainda não começaram.
- Produtores: `MessageGroupId` = `walletId` (ordem por carteira) e `MessageDeduplicationId` = `messageId`; a deduplicação do SQS cobre 5 minutos, e a inbox cobre o resto.
- Testes de integração com LocalStack e PostgreSQL: processamento e remoção, reentrega com um único efeito, rejeição de negócio removida sem DLQ, mensagens malformadas na DLQ sem nenhuma transação criada, falha transitória persistente levada à DLQ pelo redrive; no nível do caso de uso, `messageId` reutilizado, HTTP seguido de SQS e referência pendente.

## Uso do Fx

- `bootstrap.Modules()` é a lista única de módulos, usada pelo `main` e pelos testes. `fx.ValidateApp` valida o grafo sem Docker.
- Cada pacote expõe um `fx.Module`; os casos de uso são fornecidos em `bootstrap`, para que `app` não importe Fx. Os checks de readiness entram por value group (`health.AsCheck`), então um adapter novo registra o seu check sem alterar `health`.
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

LocalStack com três filas FIFO, criadas na inicialização. A fila de entrada redireciona para a DLQ após 5 recebimentos, não usa deduplicação por conteúdo (os produtores enviam ids explícitos), tem visibility timeout de 60 s e faz long polling de 20 s.

## Autenticação e autorização

- IdP: Keycloak, provisionado automaticamente pelo import do realm `wagering`. Comunicação entre serviços com `client_credentials`; o serviço nunca emite tokens nem guarda senhas.
- Cada provedor é um client confidencial cuja service account tem os papéis `wager:submit` e `wager:read`, e um mapper fixo coloca a claim `provider_id` no token. O serviço interno (`wallet-internal`) tem `wallet:admin`. Um escopo próprio adiciona a audience `wallet-api`.
- Validação (`JWTAuthenticator`, com `go-oidc`): assinatura contra o JWKS em cache (recarregado quando aparece um `kid` novo), algoritmo fixo RS256, `iss`, `aud = wallet-api` e `exp`. O Keycloak usa um hostname fixo, então o `iss` é o mesmo para tokens pedidos do host ou de dentro da rede; as réplicas buscam as chaves pela rede interna (`OIDC_JWKS_URL`).
- O `providerId` vem só do token. Um corpo com outro provedor recebe 403; a transação de outro provedor, 404; o namespace de outro provedor, 403. Chaves de idempotência são escopadas por provedor, então reusar a chave de outro provedor nunca revela o resultado dele.
- Cada rota de negócio declara sua política onde é registrada: `wager:submit` para enviar operações, `wager:read` ou `wallet:admin` para consultar transações, `wallet:admin` para carteiras. O handler só enxerga o `Principal` (client, provedor, papéis).
- Qualquer falha de autenticação é 401 sem detalhe para o cliente (o motivo vai para o log em debug), e nada é escrito no banco.
- Testes: unitários com uma chave RSA local (outra chave, HS256, expirado, audience e issuer errados, sem token) e de integração com tokens reais do Keycloak (expirado, sem audience, assinatura adulterada, isolamento entre provedores, rotas de carteira restritas).

## Imagem e Compose

- Imagem multi-stage, binários estáticos, runtime distroless sem root.
- Portas publicadas apenas em `127.0.0.1`. Bind mounts com `:z` para SELinux.
- As réplicas sobem só depois de PostgreSQL e LocalStack saudáveis e das migrations concluídas.

## Testes

- `go test ./...`: config, logging, health, router, ciclo de vida do servidor HTTP e validação do grafo Fx, sem Docker.
- `make test-integration` (build tag `integration`): a aplicação completa contra PostgreSQL e SQS reais — start, readiness verde, stop com pool fechado e porta liberada, e falha no start quando uma fila não existe.

## Limitações e trabalho não concluído

- Os testes de concorrência usam goroutines num único processo; a demonstração com três processos ainda não foi feita.
- Interpretações adotadas: `WIN` com referência ausente espera, como `REFUND`/`ROLLBACK`; `WIN` contra uma aposta já estornada recebe `ALREADY_REVERSED`; `BET` e `LOSS` com referência são rejeitados com 400; `ROLLBACK` de um `REFUND` é permitido, mas a aposta não pode ser estornada de novo.
- Com o Keycloak fora do ar e sem chaves em cache, as requisições de negócio recebem 401, não 503.
- O controle de acesso ao broker SQS por credenciais e políticas ainda não foi configurado.
- Triggers e privilégios não protegem contra um superusuário do PostgreSQL.
- Reverter a migration `000004` apaga o ledger inteiro.
- `/metrics` é público na porta da API.
- Readiness inclui o SQS, como pede o enunciado: com o SQS fora, a réplica fica *not ready* mesmo podendo atender HTTP.
- Sem `restart:` nas réplicas: uma falha transitória no boot deixa a réplica parada.
- Imagens fixadas por tag, não por digest.
