# iCare Web

Prototipe web iCare untuk pemantauan irigasi sawah. Backend dan HTML server-rendered menggunakan Go standard library; tidak ada framework JavaScript atau layanan eksternal.

## Menjalankan

```powershell
go run .
```

Buka `http://127.0.0.1:8080`, lalu buat akun dari halaman **Daftar gratis**. Secara default, server listen di `0.0.0.0:8080` agar bisa menerima koneksi dari mesin lain seperti Cloudflare Tunnel. Alamat listen dapat diubah melalui environment variable `ICARE_ADDR`; batasi akses port 8080 dengan firewall jika tidak ingin port tersebut diakses langsung dari internet.

## Cakupan prototipe

- Halaman masuk dan pendaftaran dengan validasi formulir serta kata sandi yang di-hash.
- Dashboard berisi data contoh ketinggian air, debit, tekanan, grafik 24 jam, status pompa, dan aktivitas.
- Navigasi Beranda, Riwayat, Perangkat, dan Profil.
- Kontrol mode Otomatis/Manual serta tombol pompa yang memperbarui status simulasi dan riwayat.
- Tampilan responsif untuk desktop dan ponsel.

Data sensor awal mengikuti angka dalam PDF (75 cm, 85 L/mnt, dan 1,2 bar). Nilainya adalah fixture lokal, bukan pembacaan perangkat. Kontrol pompa hanya mengubah simulasi dalam memori; belum ada integrasi ESP32, MQTT, atau perangkat fisik. Akun, sesi, dan aktivitas juga disimpan di memori dan terhapus saat server dimulai ulang. Prototipe ini tidak ditujukan untuk deployment produksi.

## Pemeriksaan

```powershell
go test ./...
```
