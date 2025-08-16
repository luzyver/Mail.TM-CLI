package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

/*
Porting dari script Python:
- Menu CLI multi-account (lihat main.py) :contentReference[oaicite:4]{index=4}
- Penyimpanan akun JSON (lihat multi_account_storage.py) :contentReference[oaicite:5]{index=5}
- Client Mail.tm: register/login/token/messages/delete/wait (lihat multi_mail_tm_client.py) :contentReference[oaicite:6]{index=6}
*/

// ========================= Storage =========================

type Account struct {
	Address   string `json:"address"`
	Password  string `json:"password"`
	AccountID string `json:"account_id"`
	Nickname  string `json:"nickname"`
}

type Storage struct {
	File     string
	Accounts map[string]Account
}

func NewStorage(file string) *Storage {
	s := &Storage{File: file, Accounts: map[string]Account{}}
	_ = s.Load()
	return s
}

func (s *Storage) Load() error {
	f, err := os.Open(s.File)
	if err != nil {
		// tidak ada file = kosong
		s.Accounts = map[string]Account{}
		return nil
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if err := dec.Decode(&s.Accounts); err != nil {
		s.Accounts = map[string]Account{}
		return err
	}
	return nil
}

func (s *Storage) Save() error {
	tmp := s.File + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.Accounts); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, s.File)
}

func (s *Storage) Add(address, password, accountID, nickname string) (string, error) {
	key := nickname
	if strings.TrimSpace(key) == "" {
		// fallback: pakai address sebagai key unik
		key = address
	}
	// jika key sudah ada & email beda, beri suffix
	if _, ok := s.Accounts[key]; ok && s.Accounts[key].Address != address {
		for i := 1; ; i++ {
			k2 := fmt.Sprintf("%s_%d", key, i)
			if _, clash := s.Accounts[k2]; !clash {
				key = k2
				break
			}
		}
	}
	s.Accounts[key] = Account{
		Address:   address,
		Password:  password,
		AccountID: accountID,
		Nickname:  nickname,
	}
	if err := s.Save(); err != nil {
		return "", err
	}
	return key, nil
}

func (s *Storage) Get(key string) (Account, bool) {
	acc, ok := s.Accounts[key]
	return acc, ok
}

func (s *Storage) Remove(key string) bool {
	if _, ok := s.Accounts[key]; !ok {
		return false
	}
	delete(s.Accounts, key)
	_ = s.Save()
	return true
}

func (s *Storage) All() map[string]Account {
	return s.Accounts
}

// optional: migrasi dari email_account.json (format lama) seperti di Python :contentReference[oaicite:7]{index=7}
func (s *Storage) MigrateFromSingle() bool {
	old := "email_account.json"
	b, err := os.ReadFile(old)
	if err != nil {
		return false
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	address := m["address"]
	password := m["password"]
	accountID := m["account_id"]
	if address == "" || password == "" {
		return false
	}
	_, _ = s.Add(address, password, accountID, "Default")
	return true
}

// ========================= Mail.tm Client =========================

type Client struct {
	BaseURL   string
	Token     string
	AccountID string
	Address   string
	Password  string
	Domain    string

	AccountKey string
	Store      *Storage
	HTTP       *http.Client
}

func NewClient(accountKey string, storageFile string) *Client {
	c := &Client{
		BaseURL: "https://api.mail.tm",
		Store:   NewStorage(storageFile),
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
	if accountKey != "" {
		_ = c.LoadAccount(accountKey)
	}
	return c
}

type domainResp struct {
	Domain string `json:"domain"`
}

type hydraDomains struct {
	Members []domainResp `json:"hydra:member"`
}

func (c *Client) GetDomains() ([]domainResp, error) {
	req, _ := http.NewRequest("GET", c.BaseURL+"/domains", nil)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("get domains failed: %s", string(body))
	}
	var hd hydraDomains
	if err := json.NewDecoder(res.Body).Decode(&hd); err != nil {
		return nil, err
	}
	return hd.Members, nil
}

func (c *Client) Register(username, password, nickname string, save bool) error {
	doms, err := c.GetDomains()
	if err != nil {
		return err
	}
	if len(doms) == 0 {
		return errors.New("no domains available")
	}
	c.Domain = doms[0].Domain

	if strings.TrimSpace(username) == "" {
		username = randomString(10)
	}
	if strings.TrimSpace(password) == "" {
		password = randomPassword(12)
	}
	c.Address = username + "@" + c.Domain
	c.Password = password

	body := map[string]string{
		"address":  c.Address,
		"password": c.Password,
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.BaseURL+"/accounts", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 201 {
		all, _ := io.ReadAll(res.Body)
		return fmt.Errorf("register failed: %s", string(all))
	}
	var acc struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(res.Body).Decode(&acc); err != nil {
		return err
	}
	c.AccountID = acc.ID

	if _, err := c.GetToken(); err != nil {
		return err
	}
	if save {
		key, err := c.SaveAccount(nickname)
		if err != nil {
			return err
		}
		c.AccountKey = key
	}
	return nil
}

func (c *Client) GetToken() (string, error) {
	body := map[string]string{
		"address":  c.Address,
		"password": c.Password,
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.BaseURL+"/token", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		all, _ := io.ReadAll(res.Body)
		return "", fmt.Errorf("token failed: %s", string(all))
	}
	var tk struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&tk); err != nil {
		return "", err
	}
	c.Token = tk.Token
	return c.Token, nil
}

func (c *Client) authReq(method, path string, body io.Reader) (*http.Request, error) {
	if c.Token == "" {
		return nil, errors.New("no token; please login first")
	}
	req, _ := http.NewRequest(method, c.BaseURL+path, body)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

type fromObj struct {
	Address string `json:"address"`
}
type message struct {
	ID        string   `json:"id"`
	From      fromObj  `json:"from"`
	Subject   string   `json:"subject"`
	CreatedAt string   `json:"createdAt"`
	HTML      any      `json:"html"` // string atau []string (API bisa variatif)
	Text      string   `json:"text"`
	Seen      bool     `json:"seen"`
}

type hydraMessages struct {
	Members []message `json:"hydra:member"`
}

func (c *Client) GetMessages() ([]message, error) {
	req, err := c.authReq("GET", "/messages", nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		all, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("get messages failed: %s", string(all))
	}
	var hm hydraMessages
	if err := json.NewDecoder(res.Body).Decode(&hm); err != nil {
		return nil, err
	}
	// mail.tm mengurutkan terbaru dulu; pastikan saja
	return hm.Members, nil
}

func (c *Client) GetMessage(id string) (*message, error) {
	req, err := c.authReq("GET", "/messages/"+id, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		all, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("get message failed: %s", string(all))
	}
	var m message
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (c *Client) DeleteMessage(id string) error {
	req, err := c.authReq("DELETE", "/messages/"+id, nil)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 204 {
		all, _ := io.ReadAll(res.Body)
		return fmt.Errorf("delete message failed: %s", string(all))
	}
	return nil
}

func (c *Client) DeleteAccount(deleteFromStorage bool) error {
	if c.AccountID == "" || c.Token == "" {
		return errors.New("no token/account id; please login first")
	}
	req, err := c.authReq("DELETE", "/accounts/"+c.AccountID, nil)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 204 {
		all, _ := io.ReadAll(res.Body)
		return fmt.Errorf("delete account failed: %s", string(all))
	}
	if deleteFromStorage && c.AccountKey != "" {
		c.Store.Remove(c.AccountKey)
	}
	c.Token = ""
	c.AccountID = ""
	c.Address = ""
	c.Password = ""
	c.AccountKey = ""
	return nil
}

func (c *Client) WaitForMessage(timeout, interval time.Duration) (*message, error) {
	start := time.Now()
	msgs, err := c.GetMessages()
	if err != nil {
		return nil, err
	}
	last := len(msgs)
	for time.Since(start) < timeout {
		time.Sleep(interval)
		cur, err := c.GetMessages()
		if err != nil {
			return nil, err
		}
		if len(cur) > last {
			// ambil paling baru
			return &cur[0], nil
		}
		last = len(cur)
	}
	return nil, nil // timeout
}

func (c *Client) SaveAccount(nickname string) (string, error) {
	if c.Address == "" || c.Password == "" {
		return "", errors.New("no account to save")
	}
	return c.Store.Add(c.Address, c.Password, c.AccountID, nickname)
}

func (c *Client) LoadAccount(key string) error {
	acc, ok := c.Store.Get(key)
	if !ok {
		return fmt.Errorf("account key not found: %s", key)
	}
	c.Address = acc.Address
	c.Password = acc.Password
	c.AccountID = acc.AccountID
	c.AccountKey = key
	_, err := c.GetToken()
	return err
}

// ========================= Utils =========================

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(letters[time.Now().UnixNano()%int64(len(letters))])
	}
	return b.String()
}

func randomPassword(length int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*"
	var b strings.Builder
	for i := 0; i < length; i++ {
		b.WriteByte(chars[time.Now().UnixNano()%int64(len(chars))])
		time.Sleep(time.Nanosecond) // ensure different seeds
	}
	return b.String()
}

func clearScreen() {
	switch runtime.GOOS {
	case "windows":
		_ = exec.Command("cmd", "/c", "cls").Run()
	default:
		fmt.Print("\033[2J\033[H")
	}
}

func header() {
	clearScreen()
	fmt.Println(strings.Repeat("=", 50))
	center("APLIKASI EMAIL SEMENTARA MAIL.TM")
	center("MULTI-ACCOUNT (Go)")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Println()
}

func center(s string) {
	if len(s) >= 50 {
		fmt.Println(s)
		return
	}
	pad := (50 - len(s)) / 2
	fmt.Println(strings.Repeat(" ", pad) + s)
}

func pause() {
	fmt.Print("\nTekan Enter untuk melanjutkan...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func openInBrowser(html string) error {
	// simpan ke temp file
	dir := os.TempDir()
	path := filepath.Join(dir, "mailtm_"+fmt.Sprint(time.Now().UnixNano())+".html")
	if err := os.WriteFile(path, []byte(html), 0644); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", "file:///"+path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

func printAccounts(store *Storage) []string {
	accts := store.All()
	keys := make([]string, 0, len(accts))
	for k := range accts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		fmt.Println("\nTidak ada akun tersimpan.")
		return keys
	}
	fmt.Println("\nDaftar akun tersimpan:")
	for i, k := range keys {
		a := accts[k]
		n := a.Nickname
		if strings.TrimSpace(n) == "" {
			n = "Tanpa nama"
		}
		fmt.Printf("%d. %s (%s)\n", i+1, a.Address, n)
	}
	return keys
}

func readLine(prompt string) string {
	fmt.Print(prompt)
	sc := bufio.NewScanner(os.Stdin)
	sc.Scan()
	return sc.Text()
}

// ========================= Menus =========================

func selectAccount(store *Storage) (string, bool) {
	header()
	fmt.Println("PILIH AKUN EMAIL")
	fmt.Println(strings.Repeat("-", 50))
	keys := printAccounts(store)
	if len(keys) == 0 {
		pause()
		return "", false
	}
	fmt.Println("\n0. Kembali ke menu utama")
	choice := readLine(fmt.Sprintf("\nPilih nomor akun (0-%d): ", len(keys)))
	if choice == "0" {
		return "", false
	}
	idx := 0
	fmt.Sscanf(choice, "%d", &idx)
	if idx < 1 || idx > len(keys) {
		fmt.Println("\nPilihan tidak valid.")
		pause()
		return selectAccount(store)
	}
	return keys[idx-1], true
}

func useAccount(store *Storage, key string) {
	header()
	fmt.Println("MENGGUNAKAN AKUN EMAIL")
	fmt.Println(strings.Repeat("-", 50))

	client := NewClient(key, "email_accounts.json")
	if client.Address == "" {
		fmt.Println("\nGagal memuat akun.")
		pause()
		return
	}

	acc, _ := store.Get(key)
	nick := acc.Nickname
	if strings.TrimSpace(nick) == "" {
		nick = "Tanpa nama"
	}
	fmt.Printf("\nAkun: %s\nEmail: %s\nPassword: %s\n", nick, client.Address, client.Password)

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println("\nOPERASI EMAIL:")
		fmt.Println("1. Cek pesan masuk")
		fmt.Println("2. Tunggu pesan baru")
		fmt.Println("3. Hapus semua pesan")
		fmt.Println("4. Ubah nickname akun")
		fmt.Println("5. Kembali ke menu utama")
		fmt.Print("\nPilih operasi (1-5): ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)

		switch line {
		case "1":
			msgs, err := client.GetMessages()
			if err != nil {
				fmt.Println("\nError:", err)
				pause()
				continue
			}
			if len(msgs) == 0 {
				fmt.Println("\nTidak ada pesan dalam kotak masuk.")
				pause()
				continue
			}
			fmt.Printf("\nDitemukan %d pesan:\n", len(msgs))
			for i, m := range msgs {
				from := m.From.Address
				if from == "" {
					from = "Unknown"
				}
				fmt.Printf("%d. Dari: %s\n   Subjek: %s\n   Tanggal: %s\n\n", i+1, from, nz(m.Subject, "No Subject"), nz(m.CreatedAt, "Unknown"))
			}
			fmt.Print("Masukkan nomor pesan untuk melihat detail (0 untuk batal): ")
			sel, _ := reader.ReadString('\n')
			sel = strings.TrimSpace(sel)
			if sel == "0" || sel == "" {
				continue
			}
			var idx int
			fmt.Sscanf(sel, "%d", &idx)
			if idx < 1 || idx > len(msgs) {
				continue
			}
			det, err := client.GetMessage(msgs[idx-1].ID)
			if err != nil {
				fmt.Println("\nError:", err)
				pause()
				continue
			}
			fmt.Println("\nDETAIL PESAN:")
			fmt.Println("Dari:", nz(det.From.Address, "Unknown"))
			fmt.Println("Subjek:", nz(det.Subject, "No Subject"))

			// cek HTML
			htmlStr := extractHTML(det.HTML)
			if htmlStr != "" {
				fmt.Println("\nPesan ini memiliki konten HTML.")
				fmt.Println("1. Lihat teks biasa")
				fmt.Println("2. Lihat HTML di browser")
				opt := readLine("Pilih opsi (1-2): ")
				if strings.TrimSpace(opt) == "2" {
					if err := openInBrowser(htmlStr); err != nil {
						fmt.Println("Gagal membuka browser:", err)
					}
				} else {
					fmt.Println("\nIsi:", nz(det.Text, htmlStr))
				}
			} else {
				fmt.Println("\nIsi:", nz(det.Text, "No Content"))
			}
			pause()

		case "2":
			to := readLine("Masukkan timeout dalam detik (default: 30): ")
			timeout := 30
			fmt.Sscanf(to, "%d", &timeout)
			if timeout <= 0 {
				timeout = 30
			}
			fmt.Printf("\nMenunggu pesan baru untuk %s...\n(Ctrl+C untuk batalkan di terminal)\n", client.Address)
			msg, err := client.WaitForMessage(time.Duration(timeout)*time.Second, 5*time.Second)
			if err != nil {
				fmt.Println("\nError:", err)
				pause()
				continue
			}
			if msg == nil {
				fmt.Println("\nTimeout: Tidak ada pesan baru diterima.")
				pause()
				continue
			}
			det, err := client.GetMessage(msg.ID)
			if err != nil {
				fmt.Println("\nError:", err)
				pause()
				continue
			}
			fmt.Println("\nPESAN BARU DITERIMA:")
			fmt.Println("Dari:", nz(det.From.Address, "Unknown"))
			fmt.Println("Subjek:", nz(det.Subject, "No Subject"))
			htmlStr := extractHTML(det.HTML)
			if htmlStr != "" {
				fmt.Println("\nPesan ini memiliki konten HTML.")
				fmt.Println("1. Lihat teks biasa")
				fmt.Println("2. Lihat HTML di browser")
				opt := readLine("Pilih opsi (1-2): ")
				if strings.TrimSpace(opt) == "2" {
					if err := openInBrowser(htmlStr); err != nil {
						fmt.Println("Gagal membuka browser:", err)
					}
				} else {
					fmt.Println("\nIsi:", nz(det.Text, htmlStr))
				}
			} else {
				fmt.Println("\nIsi:", nz(det.Text, "No Content"))
			}
			pause()

		case "3":
			yn := strings.ToLower(readLine("Apakah Anda yakin ingin menghapus semua pesan? (y/n): "))
			if yn != "y" {
				continue
			}
			msgs, err := client.GetMessages()
			if err != nil {
				fmt.Println("\nError:", err)
				pause()
				continue
			}
			if len(msgs) == 0 {
				fmt.Println("\nTidak ada pesan untuk dihapus.")
				pause()
				continue
			}
			cnt := 0
			for _, m := range msgs {
				if err := client.DeleteMessage(m.ID); err == nil {
					cnt++
				}
			}
			fmt.Printf("\n%d pesan berhasil dihapus.\n", cnt)
			pause()

		case "4":
			acc, _ := store.Get(key)
			fmt.Println("\nNickname saat ini:", nz(acc.Nickname, "Tanpa nama"))
			newNick := readLine("Masukkan nickname baru (kosongkan untuk batal): ")
			if strings.TrimSpace(newNick) == "" {
				continue
			}
			acc.Nickname = newNick
			store.Accounts[key] = acc
			_ = store.Save()
			fmt.Println("\nNickname berhasil diubah menjadi:", newNick)
			pause()

		case "5":
			return
		default:
			fmt.Println("\nPilihan tidak valid. Silakan coba lagi.")
			time.Sleep(time.Second)
		}
	}
}

func createNewEmail(store *Storage) {
	header()
	fmt.Println("MEMBUAT EMAIL BARU")
	fmt.Println(strings.Repeat("-", 50))

	custom := readLine("\nMasukkan username kustom (kosongkan untuk username acak): ")
	nick := readLine("Masukkan nickname untuk akun ini: ")

	client := NewClient("", "email_accounts.json")
	if err := client.Register(strings.TrimSpace(custom), "", strings.TrimSpace(nick), true); err != nil {
		fmt.Println("\nGagal membuat akun:", err)
		pause()
		return
	}
	fmt.Printf("\nAkun email baru berhasil dibuat dan disimpan:\nEmail: %s\nPassword: %s\nNickname: %s\n",
		client.Address, client.Password, nz(nick, "Tanpa nama"))
	pause()
}

func deleteAccount(store *Storage) {
	header()
	fmt.Println("MENGHAPUS AKUN EMAIL")
	fmt.Println(strings.Repeat("-", 50))

	key, ok := selectAccount(store)
	if !ok {
		return
	}
	acc, _ := store.Get(key)
	fmt.Printf("\nAnda akan menghapus akun:\nEmail: %s\nNickname: %s\n", acc.Address, nz(acc.Nickname, "Tanpa nama"))
	yn := strings.ToLower(readLine("\nApakah Anda yakin ingin menghapus akun ini? (y/n): "))
	if yn != "y" {
		fmt.Println("\nPenghapusan akun dibatalkan.")
		pause()
		return
	}
	client := NewClient(key, "email_accounts.json")
	if client.Address != "" {
		if err := client.DeleteAccount(true); err != nil {
			fmt.Println("\nGagal menghapus akun dari server:", err)
			fmt.Println("Menghapus dari penyimpanan lokal saja...")
			store.Remove(key)
			fmt.Printf("Akun %s berhasil dihapus dari penyimpanan lokal.\n", acc.Address)
		} else {
			fmt.Printf("\nAkun %s berhasil dihapus dari server dan penyimpanan.\n", acc.Address)
		}
	} else {
		store.Remove(key)
		fmt.Printf("\nAkun %s berhasil dihapus dari penyimpanan lokal.\n", acc.Address)
	}
	pause()
}

func showAbout() {
	header()
	fmt.Println("TENTANG APLIKASI")
	fmt.Println(strings.Repeat("-", 50))
	fmt.Println("\nAplikasi Email Sementara Mail.TM (Go)")
	fmt.Println("Versi 1.0 (Multi-Account)")
	fmt.Println("\nMenggunakan API mail.tm untuk membuat & mengelola email sementara.")
	fmt.Println("\nFitur:")
	fmt.Println("- Multi akun")
	fmt.Println("- Buat email baru")
	fmt.Println("- Simpan akun untuk pemakaian berikutnya")
	fmt.Println("- Terima & baca pesan (HTML bisa dibuka di browser)")
	fmt.Println("- Hapus pesan & akun")
	pause()
}

func mainMenu() {
	store := NewStorage("email_accounts.json")
	// migrasi dari format lama jika ada (paritas dengan Python) :contentReference[oaicite:8]{index=8}
	if _, err := os.Stat("email_account.json"); err == nil {
		if _, err2 := os.Stat("email_accounts.json"); os.IsNotExist(err2) {
			fmt.Println("Terdeteksi format akun lama. Melakukan migrasi otomatis...")
			if store.MigrateFromSingle() {
				fmt.Println("Migrasi selesai.")
				time.Sleep(2 * time.Second)
			}
		}
	}

	reader := bufio.NewReader(os.Stdin)
	for {
		header()
		ac := len(store.All())
		if ac > 0 {
			fmt.Printf("Jumlah akun tersimpan: %d\n", ac)
		} else {
			fmt.Println("Tidak ada akun tersimpan")
		}
		fmt.Println("\nMENU UTAMA:")
		fmt.Println("1. Pilih dan gunakan akun")
		fmt.Println("2. Buat akun baru")
		fmt.Println("3. Hapus akun")
		fmt.Println("4. Tentang aplikasi")
		fmt.Println("5. Keluar")
		fmt.Print("\nPilih menu (1-5): ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)

		switch line {
		case "1":
			key, ok := selectAccount(store)
			if ok {
				useAccount(store, key)
			}
		case "2":
			createNewEmail(store)
		case "3":
			deleteAccount(store)
		case "4":
			showAbout()
		case "5":
			fmt.Println("\nTerima kasih telah menggunakan aplikasi Email Sementara Mail.TM!")
			return
		default:
			fmt.Println("\nPilihan tidak valid. Silakan coba lagi.")
			time.Sleep(time.Second)
		}
	}
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("\nTerjadi kesalahan:", r)
		}
	}()
	mainMenu()
}

func nz(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func extractHTML(html any) string {
	switch v := html.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, it := range v {
			if str, ok := it.(string); ok {
				parts = append(parts, str)
			}
		}
		return strings.Join(parts, "\n")
	default:
		// beberapa response bisa []string; marshal lalu coba parse
		b, _ := json.Marshal(v)
		var arr []string
		if err := json.Unmarshal(b, &arr); err == nil {
			return strings.Join(arr, "\n")
		}
	}
	return ""
}
