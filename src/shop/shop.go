package shop

import (
	"crypto/rand"
	"log"
)

var chromeHoundsHeaderValue = [2]byte{'C', 'H'}
var xuidValue = [17]byte{
	'0', '0', '0', '0', '9', '0', '0', '0',
	'0', '4', 'E', 'A', '2', '5', '0', '6', '3'}
var unknownHeaderValue = [12]byte{
	'0',
	'0', '0', '0', '0', '0', '0', '1', 0x00, 0x00, 0x00,
	0x00,
}

type ShopHeader struct {
	ChromeHounds [2]byte
	Xuid         [17]byte
	Unknown      [12]byte
}

type ShopData struct {
	Data [5000]byte
}

type ServerState struct {
	Header ShopHeader
	Data   ShopData
}

func createHeader() ShopHeader {
	return ShopHeader{
		ChromeHounds: chromeHoundsHeaderValue,
		Xuid:         xuidValue,
		Unknown:      unknownHeaderValue,
	}
}

func createShopData() ShopData {
	var shopData = [5000]byte{}
	randomData := make([]byte, 5000)
	_, err := rand.Read(randomData)
	if err != nil {
		log.Fatalf("error while generating random string: %s", err)
	}
	copy(shopData[:], randomData)
	return ShopData{
		Data: shopData,
	}
}

func CreateShop() ServerState {
	return ServerState{
		Header: createHeader(),
		Data:   createShopData(),
	}
}

func CreateShopRaw() ServerState {
	return ServerState{
		Header: createHeader(),
		Data:   createShopData(),
	}
}
