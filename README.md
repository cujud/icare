# iCare Web

Prototipe web iCare untuk pemantauan irigasi sawah. Backend dan HTML server-rendered menggunakan Go, dengan Firebase Realtime Database sebagai layanan data perangkat.

## Menjalankan

```powershell
go run .
```

Buka `http://127.0.0.1:18081`, lalu buat akun dari halaman **Daftar gratis**. Secara default, server listen di `0.0.0.0:18081` agar bisa menerima koneksi dari mesin lain seperti Cloudflare Tunnel. Alamat listen dapat diubah melalui environment variable `ICARE_ADDR`; batasi akses port tersebut dengan firewall jika tidak ingin port tersebut diakses langsung dari internet.

## Cakupan prototipe

- Halaman masuk dan pendaftaran dengan validasi formulir serta kata sandi yang di-hash.
- Dashboard dan halaman Perangkat membaca status terbaru dari Firebase Realtime Database untuk petak yang memiliki ID perangkat.
- Halaman Riwayat mengambil maksimal 50 catatan per halaman di server menggunakan cursor Firebase; halaman berikut/sebelumnya tidak memuat seluruh riwayat.
- Dashboard menampilkan grafik pembacaan tinggi air 24 jam dan lima aktivitas terakhir dari riwayat perangkat.
- Dashboard dan Riwayat menerima pembaruan Firebase lewat WebSocket; nilai sensor, status koneksi/node/pompa, waktu pembaruan, grafik, dan aktivitas diperbarui tanpa memuat ulang halaman.
- Navigasi Beranda, Riwayat, Perangkat, dan Profil.
- Petak disimpan bersama ID perangkat Firebase opsional.
- Akun dan data petak disimpan di SQLite (`icare.db` secara default); lokasi file dapat diubah dengan `ICARE_DB_PATH`.
- Tab Notifikasi dan Pengaturan pada sidebar adalah placeholder visual dengan efek hover.
- Kontrol mode/pompa simulasi tersedia untuk petak tanpa perangkat tertaut. Perangkat Firebase menggunakan node `desired`; kontrol relay hanya aktif pada mode manual dan setelah status pompa tersedia.
- Tombol dinonaktifkan selama perintah dikirim dan tetap menunggu konfirmasi terbaru dari `status_pompa` sebelum bisa digunakan kembali.
- Tampilan responsif untuk desktop dan ponsel.

## Firebase Realtime Database

Backend membaca Firebase Realtime Database REST API mengikuti ekspor perangkat:

```json
{
  "devices": {
    "ESP32_Gateway": {
      "data_terkini": {
        "data_air_terbaru": true,
        "debit_air_lpm": 0,
        "flow_sudah_kalibrasi": false,
        "id_node": "NODE_01",
        "jarak_sensor_ke_air_cm": 23.2,
        "koneksi_lora": true,
        "pompa_menyala": false,
        "sensor_air_valid": true,
        "sensor_terhubung": true,
        "sinyal_lora_dbm": -49,
        "status_gangguan": "OK",
        "status_sensor_air": "OK",
        "tinggi_air_sawah_cm": 76.8,
        "total_volume_liter": 0,
        "waktu_update_server": 1795079698126
      },
      "desired": {
        "autoMode": true,
        "relayOn": true,
        "updatedAtMs": 1795089466821
      },
      "riwayat": {
        "-P6n4ZTLy3TdF0I-nnOP": {
          "id_node": "NODE_01",
          "keterangan_status": "MISSING_CONFIG",
          "mode_operasi": "MENUNGGU_KONFIGURASI",
          "pemicu_pencatatan": "BERKALA_1_MENIT",
          "pompa_menyala": false,
          "status_gangguan": "OK",
          "tinggi_air_sawah_cm": 76.8,
          "waktu_epoch_ms": 1795079683000,
          "waktu_lokal": "2026-11-19 17:14:43"
        }
      },
      "status_pompa": {
        "batas_mati_otomatis_cm": 70,
        "batas_nyala_otomatis_cm": 90,
        "keterangan_status": "MISSING_CONFIG",
        "mode_operasi": "MENUNGGU_KONFIGURASI",
        "pompa_menyala": false,
        "sensor_siap": true,
        "waktu_update_server": 1795079698126
      }
    }
  }
}
```

Path perangkat mengikuti struktur ekspor: `devices/{deviceID}/data_terkini`, `devices/{deviceID}/desired`, `devices/{deviceID}/riwayat`, dan `devices/{deviceID}/status_pompa`; ID perangkat pada ekspor adalah `ESP32_Gateway`. Jarak sensor hanya ditampilkan saat `sensor_air_valid` dan `data_air_terbaru` bernilai true. Halaman Riwayat meminta hingga 50 catatan per halaman dari Firebase, diurutkan dengan key Realtime Database dan dinavigasi menggunakan cursor. Dashboard menggambar titik tinggi air pada 24 jam terakhir dan menampilkan lima aktivitas terbaru. Status pompa dibaca dari `data_terkini.pompa_menyala`, dengan `status_pompa.pompa_menyala` sebagai cadangan. Nilai mode saat ini dan batas pompa otomatis dibaca dari `status_pompa`.

Kontrol perangkat menggunakan struktur `desired` yang diekspor Firebase: mode otomatis/manual ditulis ke `desired.autoMode`, status relay manual ke `desired.relayOn`, dan setiap perintah menyertakan `desired.updatedAtMs`. Update dilakukan dengan PATCH agar field lain tidak tertimpa. Mode relay hanya bisa diubah setelah perangkat melaporkan mode `MANUAL`; setelah pengiriman, tombol menunggu `status_pompa.waktu_update_server` yang lebih baru dan status mode/relay sesuai target. Backend memerlukan kredensial autentikasi Firebase dengan izin `.write`; akses baca anonim tidak pernah digunakan untuk menulis.

Dashboard dan Riwayat membuka WebSocket terautentikasi ke backend. Backend meneruskan Firebase Realtime Database streaming event untuk `data_terkini`, `status_pompa`, dan satu catatan riwayat terbaru; token Firebase tetap berada di server.

Tambahkan ID `ESP32_Gateway` pada petak di Beranda agar aplikasi membaca dan mengontrol perangkat ini.

Saat startup, aplikasi memuat `.env` dari direktori project tanpa menimpa environment variable yang sudah disetel. Untuk baca tanpa autentikasi, aktifkan opsi anonim:

```dotenv
ICARE_FIREBASE_DATABASE_URL=https://<project>-default-rtdb.<region>.firebasedatabase.app
ICARE_FIREBASE_ANONYMOUS_READ=true
```

Mode anonim tidak mengirim API key atau token; Firebase Realtime Database Rules harus mengizinkan `.read: true` pada path `devices/ESP32_Gateway`. API key hanya diperlukan untuk alur autentikasi Firebase, bukan untuk baca REST publik. Baca publik berarti siapa pun yang mengetahui URL dapat mengambil data sensor pada path tersebut. Pertahankan `.write` tetap dibatasi dan jangan membuka akses tulis publik. File `.env` diabaikan Git.

### Rules dibatasi UID (cara yang dipakai project ini)

Isi `.env` (lihat `.env.example`):

```dotenv
ICARE_FIREBASE_API_KEY=<Web API key>
ICARE_FIREBASE_EMAIL=<email akun Firebase Auth dengan UID yang diizinkan rules>
ICARE_FIREBASE_PASSWORD=<password akun tersebut>
ICARE_DEFAULT_DEVICE_ID=ESP32_Gateway
```

Backend login ke Firebase Auth (email/password) di sisi server, menyimpan ID token di memori, lalu memperbaruinya otomatis dengan refresh token. Kredensial tidak pernah dikirim ke browser. `ICARE_DEFAULT_DEVICE_ID` membuat petak tanpa ID perangkat otomatis membaca perangkat tersebut sehingga data demo tidak tampil.

Jika memakai rules yang membatasi UID, jangan aktifkan mode anonim: isi `ICARE_FIREBASE_AUTH_TOKEN` dengan Firebase ID token yang masih berlaku, atau gunakan pasangan API key dan refresh token dari akun dengan UID yang diizinkan.

Tanpa akses baca Firebase atau ID perangkat tertaut, aplikasi memakai fixture lokal (75 cm, 85 L/mnt, dan 1,2 bar) serta simulasi pompa. Sesi, akun, petak, dan aktivitas simulasi masih dikelola lokal; sesi berakhir saat server dimulai ulang. Prototipe ini tidak ditujukan untuk deployment produksi.

## Pemeriksaan

```powershell
go test ./...
```
