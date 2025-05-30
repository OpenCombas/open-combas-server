package shop

var chromeHoundsHeaderValue = [2]byte{'C', 'H'}
var xuidValue = [17]byte{
	'0', '0', '0', '0', '9', '0', '0', '0',
	'0', '4', 'E', 'A', '2', '5', '0', '6', '3'}
var unknownHeaderValue = [12]byte{
	'0',
	'0', '0', '0', '0', '0', '0', '1', 0x00, 0x00, 0x00,
	0x00,
}

// var serverData = [3000]byte{
// 	0x00 * 3000,
// }

type ShopHeader struct {
	ChromeHounds [2]byte
	Xuid         [17]byte
	Unknown      [12]byte
}

// type ShopData struct {
// 	Data [3000]byte
// }

type ServerState struct {
	Header ShopHeader
	// Data   ShopData
}

func createHeader() ShopHeader {
	return ShopHeader{
		ChromeHounds: chromeHoundsHeaderValue,
		Xuid:         xuidValue,
		Unknown:      unknownHeaderValue,
	}
}

// func createShopData() ShopData {
// 	return ShopData{
// 		Data: serverData,
// 	}
// }

func CreateShop() ServerState {
	return ServerState{
		Header: createHeader(),
		// Data:   createShopData(),
	}
}

func CreateShopRaw() ServerState {
	return ServerState{
		Header: createHeader(),
		// Data:   createShopData(),
	}
}
