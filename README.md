# watchtower

Kubernetes kümenizin **trafik akışını** ve **servis erişilebilirliğini** tek bir
web arayüzünden gösteren bir operator + UI.

- **Overview** — kümedeki namespace'leri, deployment/service/ingress'leri ve
  aralarındaki bağlantıları canlı bir harita olarak gösterir. Kartlar
  sürüklenebilir; konumlar tarayıcıda saklanır.
- **Connectivity** — bir workload seçip Service → Pod → Port zincirini kontrol
  eder; sorun varsa adım adım nedenini ve çözüm önerisini gösterir.

Hiç bilmeyen biri için iki kurulum yolu var: **A) Docker Compose** (en kolay) ve
**B) yerel geliştirme** (VSCode / terminal).

---

## Gereksinimler

- Erişilebilir bir Kubernetes kümesi (v1.25+) ve `~/.kube/config` (veya
  `KUBECONFIG` ortam değişkeni).
- **A yolu:** Docker + Docker Compose v2.
- **B yolu:** Go 1.23+, `make` (opsiyonel).

Küme erişimini doğrulamak için: `kubectl get nodes`

---

## A) Docker Compose (tek komut)

```bash
cd watchtower
LOCAL_UID=$(id -u) LOCAL_GID=$(id -g) docker compose up -d --build
```

Bu komut şunları yapar:

1. `connectivitychecks` CRD'sini kümenize kurar (idempotent),
2. manager'ı **küme dışında** çalıştırır (kubeconfig salt-okunur mount edilir),
3. UI'ı başlatır.

Ardından tarayıcıda açın: **http://localhost:8080**

Durdurmak için: `docker compose down`

> Not: kubeconfig'iniz `~/.kube/config` dışında bir yerdeyse:
> `KUBECONFIG=/yol/kubeconfig docker compose up -d --build`
> (tek dosya yolu olmalı; `:` ile ayrılmış çoklu yol desteklenmez).

---

## B) Yerel geliştirme

### VSCode ile otomatik başlatma

Projeyi VSCode'da açtığınızda `.vscode/tasks.json` içindeki görev otomatik
çalışır (ilk seferde VSCode "Allow automatic tasks in folder" diye sorar → izin
verin). Arka planda manager + UI başlar.

### Terminal ile

```bash
./scripts/dev.sh up       # derler ve arka planda başlatır
./scripts/dev.sh status   # durum + sağlık kontrolü
./scripts/dev.sh logs     # logları izle
./scripts/dev.sh down     # durdur
```

Eşdeğer make hedefleri: `make dev-up`, `make dev-status`, `make dev-logs`,
`make dev-down`.

Script şunları yapar:

- `bin/manager` ve `bin/ui` binary'lerini (gerekiyorsa) derler,
- manager'ı `:8482` API portuyla, UI'ı `:8080` ile arka planda başlatır,
- PID'leri `.dev/*.pid`, logları `.dev/*.log` altında tutar,
- sağlık kontrollerini yapar ve `http://localhost:8080` adresini yazdırır.

---

## İlk kullanım (UI)

1. **Overview** sekmesi kümenin haritasını gösterir. Namespace'e tıklayıp
   yakınlaşabilir, kartları sürükleyebilir, `fit to view` ile sıfırlayabilirsiniz.
2. **Connectivity** sekmesinde:
   - **namespace** seçin (dropdown),
   - **target** seçin (`deployment · isim` veya `pod · isim`),
   - **service** opsiyoneldir; boş bırakırsanız target'ın önündeki Service
     otomatik bulunur,
   - **run check** → zincir adım adım kontrol edilir, sonuç canlı gelir.
3. Bir karta tıklayınca alt panelde detaylar; seçili karttaki **⤢** ikonu
   bağlı kaynakların akış grafiğini açar.

---

## Sorun giderme

| Belirti | Çözüm |
|---|---|
| `http://localhost:8080` açılmıyor | `./scripts/dev.sh status` veya `docker compose ps` |
| Overview boş / "waiting for the topology mapper" | `.dev/manager.log` / `docker compose logs` — kubeconfig erişimi |
| Connectivity'de "Service not found" | Namespace/target doğru mu? Service yoksa target'ın pod'larını seçen Service otomatik aranır |
| Portlar görünmüyor | CRD'nin güncel olduğundan emin olun: `kubectl apply -f config/crd/connectivitychecks.yaml` |
| Port çakışması (8482/8080) | Başka bir manager/UI çalışıyor olabilir: `./scripts/dev.sh down` |

---

## Geliştirme

```bash
make test        # birim testleri
make build       # binary'ler
make run         # manager (out-of-cluster)
make ui          # UI sunucusu (ayrı terminal)
```

Mimari kararlar ve yol haritası: `architecture.md` · Agent kuralları: `AGENTS.md`
