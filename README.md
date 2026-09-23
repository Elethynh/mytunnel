# mytunnel

Małe CLI do wystawiania lokalnych usług HTTP pod różnymi subdomenami jednej domeny. Jeden proces `cloudflared` obsługuje wszystkie aktywne komendy. Subdomeny mapuje lokalny router, więc uruchomienie nowej usługi nie wymaga zmian DNS ani wywołania API Cloudflare.

Wymaga Go 1.22 lub nowszego do zbudowania. Gotowy plik wykonywalny nie potrzebuje runtime ani zewnętrznych bibliotek. Działa na jednym komputerze z systemem Linux lub macOS.

```bash
go build -o mytunnel .
```

Możesz skopiować plik `mytunnel` do katalogu z `PATH`, żeby wywoływać go bez `./`.

## Przed zakupem domeny: tryb lokalny

```bash
./mytunnel 3000 --subdomain demo
# http://demo.localhost:43187 → 127.0.0.1:3000
```

W drugim terminalu możesz uruchomić inną usługę:

```bash
./mytunnel 8080 --subdomain api
# http://api.localhost:43187 → 127.0.0.1:8080
```

Pominięcie `--subdomain` generuje losową nazwę. `./mytunnel status` pokazuje aktywne trasy. Każda komenda trzyma własną trasę do `Ctrl+C`; zerwanie połączenia również ją usuwa. Ostatnia zamknięta trasa zatrzymuje proces lokalny po kilku sekundach.

Jeśli system nie rozwiązuje nazw `*.localhost`, sprawdź router nagłówkiem `Host`:

```bash
curl -H 'Host: demo.localhost' http://127.0.0.1:43187/
```

## Po zakupie domeny: HTTPS przez Cloudflare

1. Dodaj domenę do Cloudflare na darmowym planie i ustaw w Google Cloud Domains serwery nazw podane przez Cloudflare. Poczekaj, aż strefa będzie aktywna i certyfikat Universal SSL zostanie wydany.
2. Zainstaluj `cloudflared` na tym komputerze. Wykonaj `cloudflared tunnel login`, potem `cloudflared tunnel create mytunnel`. Zapisz UUID tunelu i ścieżkę do wygenerowanego pliku `<UUID>.json`.
3. Skonfiguruj CLI:

   ```bash
   ./mytunnel configure \
     --domain example.com \
     --tunnel 6ff42ae2-765d-4adf-8112-31c55c1551ef \
     --credentials /pełna/ścieżka/do/6ff42ae2-765d-4adf-8112-31c55c1551ef.json
   ```

4. W Cloudflare DNS utwórz **jeden** rekord `CNAME`: nazwa `*`, cel `<UUID>.cfargotunnel.com`, status **Proxied**. Nie twórz osobnego rekordu dla każdej subdomeny.
5. Uruchom `./mytunnel 3000 --subdomain demo`. Adres będzie miał postać `https://demo.example.com`. Trasa lokalna powstaje od razu; połączenie `cloudflared` z Cloudflare może potrzebować chwili.

Konfiguracja tunelu jest generowana z `settings.json` przy starcie lokalnego procesu: ingress `*.example.com` prowadzi do routera, a pozostałe hosty otrzymują 404. `cloudflared` uruchamia się automatycznie z pierwszą trasą. Ustawienia, prywatne gniazdo sterujące i log znajdują się w `~/.config/mytunnel/` (lub w `$XDG_CONFIG_HOME/mytunnel/`). Gniazdo i `settings.json` mają uprawnienia `0600`.

Ruch publiczny wymaga uruchomionego komputera i procesu CLI. Po jego zatrzymaniu rekord DNS pozostaje; bez aktywnego tunelu Cloudflare może zwrócić błąd 1016. Wystawione usługi są publiczne i nie mają dodatkowego logowania. Narzędzie obsługuje usługi HTTP oraz połączenia WebSocket, a nazwy tylko na pierwszym poziomie domeny.

## Sprawdzenie

```bash
go test ./...
```

Testy obejmują dwa równoległe porty, zamknięcie pojedynczej trasy oraz routing HTTP i WebSocket. Działanie zewnętrznego `cloudflared` sprawdzono lokalnie z zastępczym procesem. Rzeczywistego DNS i połączenia z Cloudflare nie da się sprawdzić przed utworzeniem domeny i tunelu.

Dokumentacja Cloudflare: [lokalnie zarządzany tunel](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/create-local-tunnel/), [reguły ingress](https://developers.cloudflare.com/tunnel/features/locally-managed-tunnels/configuration-file/), [rekordy wildcard](https://developers.cloudflare.com/dns/manage-dns-records/reference/wildcard-dns-records/), [Universal SSL](https://developers.cloudflare.com/ssl/edge-certificates/universal-ssl/).
