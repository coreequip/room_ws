# FIXME — Verbesserungsplan

Umsetzungsplan aus der Code-Analyse vom 2026-07-25. Abgehakte Punkte sind umgesetzt.

## JS-Client (`example/roomws.js`)

- [x] Bug: `no_echo` wird in `publish()` verworfen, obwohl README es dokumentiert
- [x] Reconnect mit exponentiellem Backoff + Re-Subscribe bekannter Rooms nach Reconnect
- [x] `unsubscribe()`-Methode auf `Room` ergänzen (Server unterstützt es bereits)
- [x] Nachrichten vor `onopen` puffern statt in `_send()` still zu verwerfen
- [x] `JSON.parse`-Fehler in `onmessage` abfangen statt ungefangen zu werfen
- [x] `off()` zum Entfernen von Event-Listenern ergänzen
- [x] `close()`/`disconnect()`-Methode ergänzen
- [x] Modul-Export (CommonJS/ESM + weiterhin globales `<script>`-Tag), `package.json` ergänzen

## Server (`server/main.go`)

- [x] `conn.SetReadLimit()` setzen (DoS-Schutz gegen übergroße Nachrichten), Größe über `ROOMWS_MAX_MESSAGE_SIZE` konfigurierbar
- [x] Per-Client Publish-Rate-Limit (Token-Bucket pro Client), über `ROOMWS_CLIENT_PUBLISH_RATE`/`ROOMWS_CLIENT_PUBLISH_BURST` konfigurierbar
- [x] `envFloat` auf negative Werte validieren (analog zu `envInt`)
- [x] Strukturiertes Logging auf `log/slog` umstellen
- [x] `/metrics`-Endpoint (Prometheus-Textformat: uptime, clients, rooms, goroutines, memory); Mux-Aufbau dafür in `newMux()` extrahiert (testbar)
- [x] `/metrics` per Bearer-Token geschützt (`ROOMWS_METRICS_TOKEN`, sonst beim Start zufällig generiert und geloggt); im Container mit korrektem/falschem/fehlendem Token verifiziert (200/401/401)
- [x] Tests ergänzt: `envFloat`-Validierung, Read-Limit-Verhalten, `/metrics`-Inhalt + -Auth (Token generiert/aus Env, 401 ohne/mit falschem Token)
- [ ] Single-Instance-Architektur — **bewusst ausgenommen**, kein Fix

## CI/CD (`.github/workflows/docker-publish.yml`)

- [x] `pull_request`-Trigger für den `test`-Job ergänzen (Tests liefen bisher nur bei Push auf `main`); `build-and-push` läuft weiterhin nur bei `push` (Secrets/GHCR)
- [x] `go vet ./...` als CI-Schritt ergänzt
- [x] `gofmt -l` Formatierungs-Check als CI-Schritt ergänzt

## Docker (`server/Dockerfile`)

- [x] Container als Non-Root-User laufen lassen (`USER 65532:65532`, funktioniert auch in `FROM scratch` da keine NSS-Auflösung nötig ist). Mit `docker build` + `docker run` verifiziert: Prozess läuft als UID 65532, Docker-`HEALTHCHECK` wird `healthy`, `/health` und `/metrics` antworten korrekt, WebSocket-Roundtrip (handshake/subscribe/publish) funktioniert, und übergroße Nachrichten (>32 KB) schließen die Verbindung serverseitig mit Close-Code 1009.
