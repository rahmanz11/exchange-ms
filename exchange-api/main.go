package main

import (

	// for printing logs

	// for http usage

	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	// for gin framework
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	kafka "github.com/segmentio/kafka-go"

	// for postgres library
	_ "github.com/lib/pq"
	// for kafka go library
)

type Order struct {
	Blank string  `json:"blank"`
	From  string  `json:"from"`
	Fund  string  `json:"fund"`
	Amt   float64 `json:"amt"`
	Re    string  `json:"re"`
}

type ExchangeOrder struct {
	Blank         string    `json:"blank"`
	From          string    `json:"from"`
	Fund          string    `json:"fund"`
	Amt           float64   `json:"amt"`
	Re            string    `json:"re"`
	TransactionId string    `json:"transaction_id"`
	ReceivedAt    time.Time `json:"received_at"`
	ValidatedAt   time.Time `json:"validated_at"`
	SubmittedAt   time.Time `json:"submitted_at"`
}

type TransactionInit struct {
	TransactionId string    `json:"transaction_id"`
	CompletedAt   time.Time `json:"completed_at"`
}

var kafka_addr string
var db_addr string

// main function will be executed when this file is run
func main() {

	kafka_addr = getEnv("KAFKA_ADDR", "localhost:29092")
	db_addr = getEnv("DB_ADDR", "localhost:26257")

	// gin framework for REST API
	r := gin.Default()

	// API endpoints
	r.POST("/exchange_api/submit_order", PostOrder)

	// API will run at mentioned address
	r.Run(":3641")
}

// handler function for Order Post
func PostOrder(c *gin.Context) {
	received_at := time.Now()

	var order Order

	// call BindJSON to bind the received JSON to order
	if err := c.BindJSON(&order); err != nil {
		c.IndentedJSON(http.StatusBadRequest, err)
		return
	}

	msg, _ := json.MarshalIndent(order, "", "	")
	fmt.Printf("%s", string(msg))

	// database connection string
	sub_acc_db_conn := fmt.Sprintf(`postgresql://postgres:$@%s/sub_account_db?sslmode=disable`, db_addr)
	exc_ord_db_conn := fmt.Sprintf(`postgresql://postgres:$@%s/exchange_orders_db?sslmode=disable`, db_addr)

	sub_acc_db, connerr := sql.Open("postgres", sub_acc_db_conn)
	if connerr != nil {
		fmt.Printf("Error while sub account db connection %s", connerr)
		// return bad gateway
		c.IndentedJSON(http.StatusServiceUnavailable, "Could not establish sub account database connection")
		return
	}
	defer sub_acc_db.Close()

	exc_ord_db, connerr := sql.Open("postgres", exc_ord_db_conn)
	if connerr != nil {
		fmt.Printf("Error while opening exchange db connection %s", connerr)
		// return bad gateway
		c.IndentedJSON(http.StatusServiceUnavailable, "Could not establish exchange order database connection")
		return
	}
	defer exc_ord_db.Close()

	var transaction_id string
	var response string

	// validate fund account
	response = validate("Fund", order.Fund, 0, sub_acc_db)
	// response not blank means invalid request
	if response != "" {
		c.IndentedJSON(http.StatusBadGateway, response)
		return
	}

	// response not blank means invalid request
	response = validate("From", order.From, order.Amt, sub_acc_db)
	if response != "" {
		c.IndentedJSON(http.StatusBadGateway, response)
		return
	}

	validated_at := time.Now()

	// response blank means no validation error
	if response == "" {
		// generate UUID
		uuid := uuid.New()
		transaction_id = uuid.String()

		// persist to exchange_orders table
		sqlStatement := `INSERT INTO exchange_orders (transaction_id, _from, fund, amt, re, received_at, validated_at) 
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
		_, err := exc_ord_db.Exec(sqlStatement, transaction_id, order.From, order.Amt, order.Amt, order.Re, received_at, validated_at)
		if err != nil {
			c.IndentedJSON(http.StatusBadGateway, fmt.Sprintf("Could not persist exchange order record. Reason: %s", err.Error()))
			return
		} else {
			fmt.Println("New exchange order persist successful")

			// produce to exchange_orders
			conn, err := kafka.DialLeader(context.Background(), "tcp", kafka_addr, "exchange_orders", 0)
			if err != nil {
				fmt.Printf("Failed to dial leader: %s", err)
			}

			if conn != nil {
				var exchange_order ExchangeOrder
				exchange_order.Blank = order.Blank
				exchange_order.Amt = order.Amt
				exchange_order.Fund = order.Fund
				exchange_order.From = order.From
				exchange_order.Re = order.Re
				exchange_order.ReceivedAt = received_at
				exchange_order.ValidatedAt = validated_at
				exchange_order.TransactionId = transaction_id
				exchange_order.SubmittedAt = time.Now()

				// convert ExchangeOrder object to json string before publish message
				msg, _ := json.Marshal(exchange_order)
				if msg != nil {
					_, err = conn.WriteMessages(
						kafka.Message{Value: msg},
					)
					if err != nil {
						fmt.Printf("Failed to produce exchange order message. Reason: %s", err)
					}
				}
			}
		}

		// return success
		c.IndentedJSON(http.StatusCreated, transaction_id)
	}

}

func validate(type_of_acc string, acc_num string, amt float64, db *sql.DB) string {
	var msg string
	var balance float64
	var status string

	sqlStatement := `SELECT balance, status FROM sub_accounts WHERE account_number = $1`
	row := db.QueryRow(sqlStatement, acc_num)
	switch err := row.Scan(&balance, &status); err {
	case sql.ErrNoRows:
		msg = fmt.Sprintf("Sub account information invalid. Type: %s, Number: %s", type_of_acc, acc_num)
	case nil:
		// record found with given account number
		// validate account status
		if status == "inactive" {
			msg = fmt.Sprintf("Sub account status is inactive. Type: %s, Number: %s", type_of_acc, acc_num)
		} else {
			// validate from sub account balance is greater than given amt
			if type_of_acc == "From" && balance < amt {
				msg = fmt.Sprintf("Insufficient amount in sub account. Type: %s, Number: %s", type_of_acc, acc_num)
			}
		}
	default:
		msg = fmt.Sprintf("Error occurred while querying sub account database. Reason: %s", err)
	}

	return msg
}

func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}
