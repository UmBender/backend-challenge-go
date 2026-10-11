# Arquitetura

Decisões técnicas do serviço de carteiras, conforme o §15 do [enunciado](PROPOSAL.md). Como executar e testar: [README.md](README.md). Interpretações adotadas, limitações e o que não foi feito estão no fim.

## Visão geral

Um único binário (`cmd/wallet`) roda, em cada réplica, a API HTTP, o consumidor SQS, o publicador da outbox e o worker de referências pendentes. Rodam três réplicas idênticas; a corretude depende do PostgreSQL (locks, constraints, inbox, outbox), nunca de estado em memória.

```text
cmd/wallet/            ponto de entrada
cmd/healthcheck/       probe do HEALTHCHECK (imagem sem shell)
cmd/loadtest/          gerador do teste de carga
internal/domain/       money, wallet, ledger, wager (máquina de estados e regras), event; só stdlib + uuid
internal/app/          casos de uso e portas: SubmitWager, OpenWallet, consultas, reconciliação,
                       OutboxRelay, PendingWorker, hash de idempotência
internal/adapters/     postgres (TxManager, repositórios, outbox), sqsclient (consumidor, publicador),
                       httpapi (rotas, JWT, problem details)
internal/platform/     config, logging, métricas, health
internal/bootstrap/    composição Fx de todos os módulos e ciclo de vida dos workers
internal/archtest/     regras verificadas por teste: sem float, domínio sem infraestrutura, políticas do broker
migrations/            SQL versionado (up/down)
deploy/                initdb do PostgreSQL, filas e IAM do LocalStack, realm do Keycloak
tests/e2e/             cenários contra as três réplicas
scripts/               utilitário que carrega o .env.example nos testes
```

## Dinheiro

- `Money{minor int64; currency Currency}` em `internal/domain/money`: imutável, campos não exportados, construído só por `Parse`, `FromMinor` e `Zero`. No transporte é a string `"25.00"`. No banco, cada valor ocupa duas colunas: `*_minor BIGINT`, em centavos (`amount_minor`, `balance_minor`, `balance_before_minor`...), e `currency char(3)`. Os repositórios convertem com `Money.Minor()` e `money.FromMinor`, sem passar por texto nem por float.
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

### Códigos de falha

Os valores são parte do contrato público e nunca mudam de nome.

| Código | Tipo | HTTP | Quando |
| --- | --- | --- | --- |
| `INVALID_REQUEST` | corrigível, nada é gravado | 400 | campo ausente, vazio, longo demais ou de tipo errado; tipo desconhecido; referência em `BET`/`LOSS`, ausente em `REFUND`/`ROLLBACK` ou apontando para a própria operação |
| `INVALID_MONEY` | corrigível | 400 | valor fora da gramática, negativo, com escala errada, overflow ou moeda não suportada |
| `UNSUPPORTED_KIND` | corrigível | 400 | `OPENING` vindo de HTTP ou SQS (reservado à abertura interna) |
| `MISSING_IDEMPOTENCY_KEY` | corrigível | 400 | header `Idempotency-Key` ausente ou vazio |
| `INVALID_AMOUNT_FOR_KIND` | corrigível | 400 | `LOSS` diferente de `0.00`, ou valor zero nos demais tipos |
| `INSUFFICIENT_FUNDS` | definitivo, gravado como `REJECTED` | 422 | `BET` maior que o saldo |
| `REVERSAL_INSUFFICIENT_FUNDS` | definitivo | 422 | `ROLLBACK` que precisaria debitar mais que o saldo |
| `CURRENCY_MISMATCH` | definitivo | 422 | moeda da operação diferente da carteira |
| `WALLET_PLAYER_MISMATCH` | definitivo | 422 | jogador da operação diferente do dono da carteira |
| `REFERENCE_NOT_FOUND` | definitivo | 422 | referência não chegou dentro do TTL ou das tentativas |
| `REFERENCE_NOT_PROCESSED` | definitivo | 422 | referência `REJECTED`/`FAILED`, ou existente mas nunca concluída no prazo |
| `REFERENCE_MISMATCH` | definitivo | 422 | provedor, jogador, carteira, moeda ou rodada diferentes da referência |
| `REFERENCE_KIND_INVALID` | definitivo | 422 | tipo de referência não reversível pela operação |
| `REFERENCE_AMOUNT_MISMATCH` | definitivo | 422 | valor diferente do referenciado (sem estorno parcial) |
| `ALREADY_REVERSED` | definitivo | 422 | a referência já tem um estorno processado |
| `PROCESSING_FAILED` | falha permanente, gravada como `FAILED` | 500 | erro que não é de negócio, registrado só pelo worker para auditoria |

Os códigos corrigíveis vêm só da validação do request e não gravam nada, então a mesma chave pode ser reenviada depois de corrigir o corpo. Os definitivos são gravados, e um replay devolve a mesma rejeição.

## Eventos

Um tipo Go por evento, com interface selada; tipo e versão vêm do payload. O `aggregateId` de todos é a carteira, o que dá ordem por carteira na fila FIFO. O envelope é imutável, e `MarshalJSON` falha com `Money` inválido, então um evento quebrado nunca é gravado. `eventId`, correlação, causa e `occurredAt` (UTC) vêm da aplicação.

Contrato de consumo da fila `wallet-events.fifo`:

```json
{
  "eventId": "01a128b7-2e38-70e2-9581-2305a54bbcaf",
  "eventType": "WalletBalanceChanged",
  "version": 1,
  "aggregateId": "<walletId>",
  "correlationId": "provider-a:transaction-123",
  "causationId": "<requestId ou messageId>",
  "occurredAt": "2026-09-08T12:00:00.123456Z",
  "data": {
    "walletId": "<walletId>", "transactionId": "<transactionId>", "direction": "DEBIT",
    "money": {"amount": "25.00", "currency": "BRL"},
    "balanceBefore": {"amount": "1000.00", "currency": "BRL"},
    "balanceAfter": {"amount": "975.00", "currency": "BRL"},
    "walletVersion": 2
  }
}
```

| `eventType` | `data` |
| --- | --- |
| `WagerTransactionProcessed` | `transactionId`, `walletId`, `playerId`, `kind`, `money`, `balance`, `walletVersion`; para operações externas também `providerId`, `externalTransactionId`, `roundId`, `gameId` e `referenceTransactionId` quando houver |
| `WagerTransactionRejected` | `transactionId`, `walletId`, `playerId`, `kind`, `money`, `providerId`, `externalTransactionId`, `failureCode` |
| `WalletBalanceChanged` | `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter`, `walletVersion` |
| `WagerTransactionPendingReference` | `transactionId`, `walletId`, `playerId`, `kind`, `money`, `providerId`, `externalTransactionId`, `referenceExternalTransactionId`, `expiresAt` |

Roteamento: `MessageGroupId` = `aggregateId` (ordem por carteira) e `MessageDeduplicationId` = `eventId`. Consumidores devem deduplicar por `eventId` (entrega pelo menos uma vez), desviar por `eventType` e `version`, e podem usar `walletVersion` para descartar um saldo mais antigo que o já visto.

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

Corpos de resposta de `POST /wagering/transactions`:

```text
201 {"transactionId":"…","status":"PROCESSED","balance":{"amount":"975.00","currency":"BRL"},"idempotentReplay":false}
200 {"transactionId":"…","status":"PROCESSED","balance":{"amount":"975.00","currency":"BRL"},"idempotentReplay":true}
202 {"transactionId":"…","status":"PENDING_REFERENCE","idempotentReplay":false}      + Location: /wagering/transactions/…
422 {"transactionId":"…","status":"REJECTED","failureCode":"INSUFFICIENT_FUNDS","idempotentReplay":false}
400 {"type":"about:blank","title":"Bad Request","status":400,"detail":"…","code":"INVALID_MONEY","correlationId":"…"}
409 {"type":"about:blank","title":"Conflict","status":409,"detail":"…","code":"IDEMPOTENCY_KEY_REUSED","correlationId":"…"}
503 {"type":"about:blank","title":"Service Unavailable","status":503,"detail":"…","code":"SERVICE_UNAVAILABLE","correlationId":"…"}  + Retry-After: 1
```

Os resultados de uma operação (processada, pendente, rejeitada) usam o corpo da transação, e o replay devolve o mesmo corpo com `idempotentReplay: true`. Os erros de requisição usam `application/problem+json`. Uma rejeição de negócio não traz saldo: não houve movimentação.

- Cada requisição de negócio tem `HTTP_REQUEST_TIMEOUT` (10 s, menor que o write timeout); o prazo chega ao banco pelo context.
- O access log registra método, rota, status, duração, correlation id, request id, client e provedor, nunca corpos ou headers.
- O ledger é paginado com cursor opaco (`base64url` de JSON com o último `seq`), estável enquanto novos lançamentos chegam.

## Outbox e publicação de eventos

- Os eventos são gravados em `outbox_events` na mesma transação da mudança que os causou. Um evento existe se e somente se a mudança foi commitada, então nunca é publicado antes do commit nem perdido depois dele.
- Toda réplica roda um `OutboxRelay`. A cada ciclo ele reivindica até 50 eventos devidos numa única instrução: `SELECT … FOR UPDATE SKIP LOCKED` dentro de um `UPDATE` que grava o lease (`locked_by`, `locked_until` = 30 s). Publicadores concorrentes pegam lotes disjuntos, e nenhum lock fica aberto enquanto o evento viaja até o broker.
- Publicação em `wallet-events.fifo` com `SendMessageBatch` (até 10 eventos por chamada; dentro de um lote o SQS mantém a ordem de cada grupo), `MessageGroupId` = carteira (ordem por carteira) e `MessageDeduplicationId` = `eventId`. Os eventos enviados são marcados numa única instrução por lote. O teste de carga mostrou que enviar um a um não acompanhava a produção de eventos. O payload é o envelope gravado; por ser `jsonb`, os espaços e a ordem das chaves podem diferir, mas o conteúdo é o mesmo.
- Sucesso: `published_at`, só nas linhas que ainda pertencem ao lease do publicador. Uma falha parcial do lote afeta só os eventos que falharam. Falha: `attempts + 1`, `last_error`, próximo horário com backoff exponencial e jitter (1 s a 1 min) e lease liberado. O evento nunca é descartado; o relay tenta até o broker voltar.
- Entrega pelo menos uma vez. Se o processo morrer entre o envio e a marcação, o lease expira e outra réplica reenvia o mesmo `eventId`; a deduplicação do SQS absorve reenvios dentro da janela, e os consumidores devem deduplicar por `eventId`. A ordem por carteira é a ordem de `occurred_at` no caminho normal; após uma falha, um evento pode chegar depois de um mais novo, e `walletVersion` permite detectar isso.
- O dono do lease é `INSTANCE_ID` mais um sufixo aleatório por processo, para que uma réplica reiniciada não feche linhas reivindicadas pela encarnação anterior. No shutdown o loop é cancelado antes do pool fechar.
- Métricas: `outbox_events_published_total`, `outbox_publish_failures_total` e `outbox_lag_seconds` (idade do evento mais antigo não publicado).
- Testes de integração com PostgreSQL e LocalStack reais: publicação dos eventos commitados (grupo e deduplicação corretos), dois publicadores concorrentes enviando cada evento exatamente uma vez, broker fora do ar (nada perdido, reagendado e publicado na volta), lease expirado retomado por outro publicador (crash entre o commit e o envio) e crash entre o envio e a marcação (reenvio com o mesmo `eventId`, deduplicado pelo SQS).

## Consumidor SQS e inbox

- Toda réplica consome `wager-transactions.fifo` com long polling de 5 s e até 10 mensagens por vez. Mensagens do mesmo `MessageGroupId` são processadas em ordem; grupos diferentes, em paralelo (até 4).
- O envelope do §10 é lido de forma estrita (campos desconhecidos rejeitados, UUIDs canônicos, dinheiro como string) e vira o mesmo `SubmitCommand` do HTTP, com a chave `data.idempotencyKey`: validação, hash e garantias financeiras são as do caminho HTTP.
- A inbox `(consumer_name, message_id)` é gravada e concluída na mesma transação SQL das mudanças de domínio, do ledger e dos eventos. Uma reentrega encontra a linha concluída e responde com o resultado armazenado, sem novo efeito; o mesmo `messageId` com outro conteúdo (hash do payload e da chave) é permanente e vai para a DLQ. Uma operação enviada por HTTP e depois por SQS é um replay. Uma referência pendente conclui a mensagem assim que a pendência é persistida; o worker continua a partir daí.
- A mensagem só é removida depois do commit. Se o processo morrer entre o commit e a remoção, a reentrega é um replay.
- Classificação:
  - sucesso, replay ou rejeição de negócio → remove;
  - mensagem malformada, request inválido, chave reutilizada, external id com outra chave, carteira inexistente, `messageId` reutilizado ou violação de invariante → encaminha explicitamente para `wager-transactions-dlq.fifo` (com o motivo em `failureReason`) e remove;
  - indisponibilidade, conflito esgotado, timeout ou erro desconhecido → torna a mensagem visível de novo com backoff (2 s a 30 s), sem processar as seguintes do mesmo grupo; após `maxReceiveCount = 5` recebimentos a política de redrive leva a mensagem para a DLQ.
- Limites: visibility timeout de 60 s na fila e 10 s por mensagem no processamento, então uma mensagem nunca é reentregue enquanto ainda está sendo tratada.
- `SIGTERM`: para de buscar mensagens, conclui as que estão em andamento (sem cancelá-las) e libera a visibilidade das que ainda não começaram. O long poll em andamento não é cancelado: o broker ainda poderia entregar mensagens a uma requisição abandonada, e elas ficariam invisíveis até o visibility timeout. Ele termina em até 5 s, e o que chegar depois do início do shutdown é liberado na hora. Os testes e2e encontraram esse caso: uma mensagem enviada logo após um restart ficava parada 60 s.
- Produtores: `MessageGroupId` = `walletId` (ordem por carteira) e `MessageDeduplicationId` = `messageId`; a deduplicação do SQS cobre 5 minutos, e a inbox cobre o resto.
- Testes de integração com LocalStack e PostgreSQL: processamento e remoção, reentrega com um único efeito, rejeição de negócio removida sem DLQ, mensagens malformadas na DLQ sem nenhuma transação criada, falha transitória persistente levada à DLQ pelo redrive, e um consumidor que morre depois do commit e antes da remoção (a mensagem volta e é concluída como replay, com um débito só); no nível do caso de uso, `messageId` reutilizado, HTTP seguido de SQS, HTTP e SQS ao mesmo tempo (20 entregas, um efeito) e referência pendente.

## Uso do Fx

- `bootstrap.Modules()` é a lista única de módulos, usada pelo `main` e pelos testes. `fx.ValidateApp` valida o grafo sem Docker.
- Cada pacote expõe um `fx.Module`; os casos de uso são fornecidos em `bootstrap`, para que `app` não importe Fx. Os checks de readiness entram por value group (`health.AsCheck`), então um adapter novo registra o seu check sem alterar `health`.
- `config.Module` fornece as sub-structs (`config.HTTP`, `config.Database`...), para que cada construtor dependa só do que usa.
- A ordem de start e stop vem do grafo: o Fx executa `OnStart` na ordem de construção e `OnStop` ao contrário. O pool do PostgreSQL é construído antes do servidor HTTP, logo fecha depois dele, sem ordenação manual.
- Só `cmd/`, `bootstrap` e os arquivos de módulo importam Fx; o domínio não importa Fx, pgx, net/http nem AWS, e `internal/archtest` garante isso.
- Os workers (consumidor SQS, publicador da outbox, worker de pendências) rodam por `runInBackground`: um hook `OnStart` inicia o loop numa goroutine com um context próprio; o `OnStop` cancela esse context e espera o loop terminar, dentro do prazo do Fx. Como cada worker depende do pool e das filas pelo construtor, ele inicia depois deles e para antes deles.

## Ciclo de vida e shutdown

- **Start:** o `OnStart` faz `Ping` no PostgreSQL e resolve as três filas; falha em qualquer um aborta o processo (fail fast). O servidor faz `net.Listen` síncrono, então porta ocupada também falha o start. Se o `Serve` morrer depois, a aplicação inteira encerra com código 1.
- **Stop (SIGTERM),** em ordem inversa à do start:
  1. readiness passa a 503;
  2. o HTTP para de aceitar conexões e drena as requisições em andamento;
  3. o consumidor SQS para de buscar mensagens, conclui as que estão em andamento e libera a visibilidade do resto;
  4. o publicador da outbox e o worker de pendências cancelam a iteração atual (a transação desfaz, ou o evento é reenviado depois com o mesmo `eventId`);
  5. o pool do PostgreSQL fecha;
  6. exit 0.
- **Orçamentos encadeados:** `HTTP_SHUTDOWN_TIMEOUT` (10 s) < `SHUTDOWN_TIMEOUT` (25 s, stop do Fx) < `stop_grace_period` (30 s, Compose). A config rejeita valores fora dessa ordem. O consumidor espera no máximo o long poll (5 s) e o processamento em andamento (10 s); na prática, `docker compose stop` de uma réplica leva cerca de 2 s.

## Configuração

Variáveis de ambiente lidas com a stdlib e validadas no start; todos os erros são reportados de uma vez. Credenciais AWS ficam com a cadeia padrão do SDK, fora das structs da aplicação. `DATABASE_URL` contém senha e nunca é logada nem incluída em mensagens de erro.

## Health checks e observabilidade

- `GET /health/live` não consulta dependências: uma oscilação do banco não deve reiniciar todas as réplicas.
- `GET /health/ready` executa os checks de PostgreSQL e SQS em paralelo, com 1 s de timeout cada. A resposta diz apenas `ok`/`unavailable`; o erro detalhado vai para o log.
- Logs JSON (`log/slog`), sempre com `service` e `instanceId`. Cada operação registra `correlationId`, `transactionId`, `walletId`, `providerId`, o status e se foi replay; o consumidor acrescenta `messageId`. O access log tem método, rota, status, duração, correlation id, request id, client e provedor. Corpos, headers, tokens e saldos nunca vão para o log. Os eventos internos do Fx saem no mesmo formato.
- Métricas Prometheus em `/metrics`, com registry dedicado e labels de cardinalidade limitada:

| Métrica | O que mede |
| --- | --- |
| `wager_operations_total{origin,result}` | resultados por origem (`http`, `sqs`, `worker`) e status (`PROCESSED`, `REJECTED`, `PENDING_REFERENCE`, `FAILED`) ou classe de erro |
| `wager_duplicates_total{origin}` | repetições respondidas com o resultado armazenado |
| `wager_processing_duration_seconds{origin}` | latência de processamento, retries incluídos |
| `wager_concurrency_conflicts_total` | transações repetidas após conflito detectado pelo banco |
| `wager_pending_reference_retries_total` | buscas de referência que reagendaram a operação |
| `sqs_messages_total{outcome}` | mensagens `processed`, `dlq` (encaminhadas) e `retry` (visíveis de novo) |
| `outbox_events_published_total`, `outbox_publish_failures_total`, `outbox_lag_seconds` | publicação e atraso da outbox |
| `wallet_reconciliation_divergence_total` | divergências de reconciliação |
| `http_request_duration_seconds{route,method,code}` | latência HTTP por padrão de rota, nunca pelo caminho bruto |

- Mensagens movidas para a DLQ pelo redrive da fila (tentativas esgotadas) não passam pelo consumidor; elas aparecem como `sqs_messages_total{outcome="retry"}` antes e na profundidade da DLQ no broker.

## PostgreSQL e migrations

- Driver `pgx/v5` com `pgxpool`; até 10 conexões por réplica.
- Dois roles, criados pelo initdb na primeira inicialização do volume:
  - `wallet_owner` é dono do banco e roda as migrations; não é superusuário, então triggers também o restringem;
  - `wallet_app` é o role da aplicação e só recebe os grants que cada migration concede explicitamente.
- `000001` revoga os privilégios de `PUBLIC` no schema e concede apenas `USAGE` a `wallet_app`.
- `golang-migrate`, com `.up.sql` e `.down.sql` separados, roda como job único no Compose antes das réplicas.

## Mensageria

LocalStack com três filas FIFO, criadas na inicialização. A fila de entrada redireciona para a DLQ após 5 recebimentos, não usa deduplicação por conteúdo (os produtores enviam ids explícitos), tem visibility timeout de 60 s e o consumidor faz long polling de 5 s.

### Controle de acesso ao broker

O §2 pede acesso à mensageria controlado por credenciais e políticas do broker. Cada componente tem uma identidade própria, com menor privilégio, em `deploy/localstack/iam/`:

| Identidade | Pode |
| --- | --- |
| `wallet-service` (as réplicas) | resolver as três filas; receber, apagar e mudar a visibilidade na fila de entrada; enviar à DLQ e à fila de eventos |
| `wager-producer` (provedores) | enviar apenas à fila de entrada |
| `events-consumer` (consumidores de eventos) | receber e apagar apenas na fila de eventos |

Duas camadas, como na AWS: políticas de identidade (usuários IAM criados por `05-iam.sh`) e queue policies (resource-based) em cada fila, que só admitem os principals esperados. Ninguém tem `PurgeQueue`, `DeleteQueue` ou `SetQueueAttributes`. As réplicas usam a credencial `wallet-service`.

**Limitação:** o LocalStack Community grava essas políticas, mas não as aplica, nem com `ENFORCE_IAM=1` (verificado: uma chave com permissão só de `SendMessage` conseguiu ler a fila). Por isso, a garantia é verificada de dois outros modos:
- `internal/archtest/broker_policy_test.go` avalia os mesmos arquivos JSON com um avaliador mínimo de IAM (deny explícito vence; allow com wildcards; principal nas queue policies) e compara com a matriz completa: cada identidade × cada ação × cada fila. Uma permissão a mais ou a menos faz o teste falhar;
- um teste de integração confere que as três filas provisionadas carregam exatamente essas policies.

As validações de domínio continuam no consumidor, como o §2 exige: mesmo uma mensagem enviada por quem não deveria passa por validação estrita, idempotência, inbox e regras de negócio.

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
- As réplicas sobem só depois de PostgreSQL e LocalStack saudáveis e das migrations concluídas, e usam `restart: on-failure`.
- O healthcheck do PostgreSQL usa TCP: durante o initdb, o entrypoint roda um servidor temporário só no socket Unix e depois o reinicia. Um check pelo socket ficaria verde cedo demais, e as migrations cairiam nesse reinício.
- O healthcheck do LocalStack só fica verde quando os scripts de IAM e filas terminaram sem erro. O Keycloak usa `KC_HOSTNAME` fixo, para o `iss` dos tokens ser o mesmo dentro e fora da rede.

## Testes

- `go test ./...` (sem Docker): dinheiro (com fuzz), domínio e máquina de estados, regras de arquitetura (sem float, domínio sem infraestrutura), config, HTTP (mapeamento de erros, decodificação estrita, autorização, JWT com chave local), relay do outbox e parsing de mensagens SQS.
- `make test-integration` (build tag `integration`, PostgreSQL, LocalStack e Keycloak reais): constraints e triggers por SQLSTATE, casos de uso e concorrência (goroutines com barreira de largada), HTTP de ponta a ponta, tokens reais, outbox, worker de pendências, consumidor SQS e o ciclo de vida da aplicação completa. Um banco isolado por teste.
- `make test-e2e` (build tag `e2e`): cenários caixa-preta contra as três réplicas do Compose, que são processos separados, incluindo uma réplica morta com `SIGKILL` sob carga e todas as réplicas mortas e religadas com uma operação já processada e outra pendente. Cada cenário termina com a reconciliação saldo × ledger.
- `make test-load`: teste de carga contra as três réplicas (seção seguinte).
- O ensaio de entrega é feito a partir de um clone limpo: `docker compose up --build`, depois `make check`, `make test-integration`, `make test-e2e` e `make test-load`. Foi esse ensaio que revelou o script de filas sem permissão de execução e o healthcheck do PostgreSQL que passava durante o initdb, ambos corrigidos.

### Mapa dos testes obrigatórios (§13)

| Exigência | Onde |
| --- | --- |
| `Money`: parsing, escala, limites, entradas inválidas, moedas | `internal/domain/money/*_test.go` (inclui fuzz) |
| invariantes da carteira, transições, regras dos cinco tipos, política de zero, abertura com metadados e eventos | `internal/domain/{wallet,wager,ledger,event}/*_test.go` |
| conflito de payload para a mesma chave | `internal/app/submit_wager_integration_test.go` (`TestIdempotencyConflicts`) |
| migrations, constraints, imutabilidade do ledger | `internal/adapters/postgres/schema_integration_test.go` |
| atomicidade financeira, reentrega, inbox | `internal/app/{submit_wager,inbox}_integration_test.go` |
| outbox concorrente, retry, recuperação de publicação | `internal/app/outbox_relay_integration_test.go` |
| DLQ e retry no consumidor | `internal/adapters/sqsclient/consumer_integration_test.go` |
| composição Fx, início, encerramento e liberação de recursos | `internal/bootstrap/*_test.go` |
| IdP real; credenciais ausentes, inválidas, expiradas; isolamento entre provedores; operações internas restritas | `internal/adapters/httpapi/keycloak_integration_test.go` |
| 1. a mesma aposta 50× em paralelo | `TestSameBet50TimesConcurrently`; e 30× entre as réplicas em `tests/e2e` |
| 2. 80.00 + 80.00 sobre 100.00 | `TestTwoConcurrentBetsOf80On100`; e entre réplicas em `tests/e2e` |
| 3. carteiras distintas ao mesmo tempo | `TestIndependentWalletsDoNotBlockEachOther`, `TestManyWalletsInParallel` |
| 4. cenários com três instâncias independentes | `tests/e2e` (três réplicas do Compose) |
| 5. consumidor interrompido depois do commit e antes da remoção | `TestConsumerCrashAfterCommitBeforeDelete` |
| 6. dois publishers na mesma outbox, recuperação | `TestOutboxRelayConcurrentPublishersSendEachEventOnce`, `…ReclaimsAnExpiredLease`, `…RepublishesWithTheSameEventID` |
| 7. `REFUND` antes da referência: resolução e expiração | `internal/app/pending_worker_integration_test.go`; e entre réplicas em `tests/e2e` |
| 8. reinício preservando idempotência e pendências | `TestRestartPreservesIdempotencyAndPendings` (`tests/e2e`) |
| HTTP e SQS para a mesma operação | `TestHTTPThenSQSIsOneOperation`, `TestHTTPAndSQSConcurrently`, e `tests/e2e` |
| saldo × ledger no fim | `assertReconciled` nos testes de integração e e2e; o teste de carga reconcilia todas as carteiras |

Todos rodam com `-race` (`make check`, `make test-integration`).

## Teste de carga

**Comando:** `make test-load`, com a stack no ar (`docker compose up --build -d`). Equivale a `go run ./cmd/loadtest -duration 60s -workers 32`; as flags permitem mudar a duração, os workers, as carteiras, a fração de reenvios e a de carteira quente.

**Ambiente:** um notebook (Intel i5-8250U, 4 núcleos / 8 threads, 7 GiB de RAM, Fedora 44, Docker 29.7.2) rodando **tudo junto**: PostgreSQL, LocalStack, Keycloak, as três réplicas (pool de 10 conexões cada) e o próprio gerador de carga. Os números valem como referência relativa, não como capacidade de produção.

**Metodologia:**
- abre 50 carteiras e mais uma "quente";
- 32 clientes concorrentes enviam `BET`s de 1.00 por HTTP, com tokens reais do Keycloak, em rodízio entre as três réplicas, por 60 s;
- 20% das apostas novas vão para a carteira quente, para gerar disputa pelo lock;
- 10% das requisições reenviam uma operação já processada, com a mesma chave, para medir duplicatas;
- a latência é medida no cliente; conflitos, duplicatas e outbox vêm das métricas Prometheus das réplicas (diferença entre antes e depois); o atraso da outbox é amostrado a cada segundo;
- no fim, espera a outbox esvaziar e reconcilia todas as carteiras, conferindo também se cada saldo é igual ao inicial menos as apostas aceitas.

**Resultado (60 s, 32 workers):**

| Item | Valor |
| --- | --- |
| Throughput | 18.347 requisições em 60,4 s → **303,7 req/s** (273,2 apostas novas/s) |
| Latência | p50 34,1 ms · p95 477,5 ms · p99 1.065,5 ms · máx 4.735,2 ms |
| Status | 201: 16.508 · 200 (replay): 1.839 |
| Erros | 0 (nenhuma resposta fora de 2xx e nenhum erro de transporte) |
| Duplicatas | 1.839 respondidas como replay, igual à métrica do servidor |
| Conflitos de concorrência | 0 |
| Outbox | 33.056 eventos publicados · atraso máximo 4,77 s · vazia 0,8 s após o fim da carga |
| Integridade | 51/51 carteiras consistentes e com o saldo esperado |

Os números variam entre execuções nesta máquina compartilhada: uma segunda rodada, feita logo depois dos suítes de integração e e2e num clone limpo, deu 186,1 req/s, p95 781,9 ms e p99 1.826,8 ms. Também teve zero erros, atraso máximo da outbox de 6,0 s e fila vazia 1,1 s depois, com 51/51 carteiras íntegras. O que se mantém entre execuções é a ausência de erros, a integridade financeira e a outbox acompanhando a carga.

**Leitura:**
- Zero conflitos é esperado: o lock pessimista por carteira faz a disputa virar **espera** (e latência), não `40001`. A cauda (p95/p99) vem principalmente da carteira quente, que recebe 20% das apostas e é serializada. Carteiras diferentes seguem em paralelo.
- A primeira medição revelou a outbox como gargalo: com um `SendMessage` e um `UPDATE` por evento, o publicador não acompanhava os dois eventos de cada aposta (atraso de 49 s, e a fila ainda não tinha esvaziado 2 min depois da carga, com 204 req/s). Com `SendMessageBatch` (10 por chamada) e uma única marcação por lote, o atraso caiu para 4,8 s, a fila passou a esvaziar em menos de 1 s, e o throughput subiu 49%, porque sobrou CPU para a API na mesma máquina.

## Limitações e trabalho não concluído

- Interpretações adotadas: `WIN` com referência ausente espera, como `REFUND`/`ROLLBACK`; `WIN` contra uma aposta já estornada recebe `ALREADY_REVERSED`; `BET` e `LOSS` com referência são rejeitados com 400; `ROLLBACK` de um `REFUND` é permitido, mas a aposta não pode ser estornada de novo.
- Com o Keycloak fora do ar e sem chaves em cache, as requisições de negócio recebem 401, não 503.
- **As políticas do broker não são aplicadas localmente.** Elas estão provisionadas (usuários IAM, políticas de identidade e queue policies), mas o LocalStack Community não as aplica, nem com `ENFORCE_IAM=1`: testei uma chave com permissão só de `SendMessage` e ela conseguiu ler a fila. O menor privilégio é verificado por um teste que avalia as políticas, não pelo broker. Em AWS, as mesmas políticas seriam aplicadas, e as réplicas usariam uma IAM role em vez das chaves estáticas `wallet-service`.
- Triggers e privilégios não protegem contra um superusuário do PostgreSQL.
- Reverter a migration `000004` apaga o ledger inteiro.
- `/metrics` é público na porta da API.
- Readiness inclui o SQS, como pede o enunciado: com o SQS fora, a réplica fica *not ready* mesmo podendo atender HTTP.
- As réplicas usam `restart: on-failure` (voltam após falha de boot ou crash); um `docker kill` manual não é reiniciado pelo Docker.
- Não há injeção de falha dentro do processo (por exemplo, morrer exatamente entre o commit e a remoção da mensagem). Esses pontos são cobertos pelos efeitos equivalentes: reentrega de mensagem já commitada, lease de outbox expirado e réplica morta com `SIGKILL`.
- O teste de carga roda numa única máquina, junto com toda a infraestrutura; não representa a capacidade de produção.
- Imagens fixadas por tag, não por digest.
