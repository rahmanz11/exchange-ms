package main

import (

	// for printing logs

	// for http usage

	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	// for gin framework
	kafka "github.com/segmentio/kafka-go"

	// for postgres library
	_ "github.com/lib/pq"
	// for kafka go library
)

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

	kafka_addr = getEnv("KAFKA_ADDR", "kafka:29092")
	db_addr = getEnv("DB_ADDR", "node_1:26257")

	// initialize kafka connection and reader
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:   []string{kafka_addr},
		GroupID:   "fund-group",
		Topic:     "exchange_orders",
		Partition: 0,
		MinBytes:  10e3, // 10KB
		MaxBytes:  10e6, // 10MB,
		MaxWait:   1 * time.Second,
	})

	if r != nil {

		ctx := context.Background()
		sub_acc_db_conn := fmt.Sprintf(`postgresql://postgres:$@%s/sub_account_db?sslmode=disable`, db_addr)
		sub_acc_db, connerr := sql.Open("postgres", sub_acc_db_conn)
		// run infinitely and fetch messages when available
		for {
			m, err := r.FetchMessage(ctx)
			if err != nil {
				fmt.Printf("error in fetch msg %s\n", err)
				continue
			}

			var exchange_order ExchangeOrder
			// unmarshal json string
			json.Unmarshal(m.Value, &exchange_order)

			// validate
			if connerr != nil {
				fmt.Printf("Error while sub account db connection %s", connerr)
				sub_acc_db.Close()
				continue
			}

			val, _ := json.MarshalIndent(exchange_order, "", "	")
			fmt.Printf("%s", string(val))

			msg := validate(exchange_order.From, exchange_order.Amt, sub_acc_db)
			if msg != "" {
				update_transaction_failed(exchange_order.TransactionId, msg)
				continue
			}

			// database connection string
			transaction_db_conn := fmt.Sprintf(`postgresql://postgres:$@%s/transactions_db?sslmode=disable`, db_addr)
			db, connerr := sql.Open("postgres", transaction_db_conn)
			if connerr != nil {
				fmt.Printf("Error while opening transactions database connection. Reason: %s", connerr)
			} else {
				sqlStatement := `INSERT INTO transactions (transaction_id, _from, fund, amt, re, received_at, 
								validated_at, submitted_at, completed_at, status, system_notes) 
								VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
				completed_at := time.Now()
				_, err := db.Exec(sqlStatement, exchange_order.TransactionId, exchange_order.From, exchange_order.Amt,
					exchange_order.Amt, exchange_order.Re, exchange_order.ReceivedAt, exchange_order.ValidatedAt,
					exchange_order.SubmittedAt, completed_at, "completed", "default note")

				if err != nil {
					fmt.Printf("Error while inserting data into transactions table. Reason: %s", err.Error())
				} else {
					// update sub account balance
					adjust_sub_account_balance(exchange_order.From, exchange_order.Fund, exchange_order.Amt)
				}
			}
		}
		sub_acc_db.Close()
	}
}

func update_transaction_failed(transaction_id string, msg string) {
	transaction_db_conn := fmt.Sprintf(`postgresql://postgres:$@%s/transactions_db?sslmode=disable`, db_addr)

	db, connerr := sql.Open("postgres", transaction_db_conn)
	if connerr != nil {
		fmt.Printf("Fund-Consumer: Error while opening transactions db con %s\n", connerr)
	} else {
		sql_statement := `UPDATE transactions SET status = $1 WHERE transaction_id = $2;`
		res, err := db.Exec(sql_statement, "cancelled", transaction_id)

		if err != nil {
			fmt.Printf("Fund-Consumer: Error occurred while updating transactions record. Reason: %s", err.Error())
		} else {
			count, err := res.RowsAffected()
			if err != nil {
				fmt.Printf("Fund-Consumer: Error occurred while getting rows affected of transactions. Reason: %s", err.Error())
			} else {
				if count > 0 {
					fmt.Printf("Fund-Consumer: Transaction updated, id: %s", transaction_id)
				} else {
					fmt.Printf("Fund-Consumer: No transaction row updated")
				}
			}
		}
	}
	db.Close()
}

func adjust_sub_account_balance(from string, fund string, amt float64) {
	sub_acc_db_conn := fmt.Sprintf(`postgresql://postgres:$@%s/sub_account_db?sslmode=disable`, db_addr)

	sub_acc_db, connerr := sql.Open("postgres", sub_acc_db_conn)
	if connerr != nil {
		fmt.Printf("Error while sub account db connection %s", connerr)
		return
	}
	defer sub_acc_db.Close()

	if connerr != nil {
		fmt.Printf("Fund-Consumer: Error while opening sub account db con %s\n", connerr)
	} else {
		sql_statement := `UPDATE sub_accounts SET balance = balance - $1 WHERE sub_account_id = $2;`
		res, err := sub_acc_db.Exec(sql_statement, amt, from)

		if err != nil {
			fmt.Printf("Fund-Consumer: Error occurred while updating balance in from sub account. Reason: %s", err.Error())
		} else {
			count, err := res.RowsAffected()
			if err != nil {
				fmt.Printf("Fund-Consumer: Error occurred while adjusting from account %s balance. Reason: %s", from, err.Error())
			} else {
				if count > 0 {
					fmt.Printf("Fund-Consumer: From account balance adjusted, id: %s", from)
					sql_statement := `UPDATE sub_accounts SET balance = balance + $1 WHERE sub_account_id = $2;`
					res, err := sub_acc_db.Exec(sql_statement, amt, fund)
					if err != nil {
						fmt.Printf("Fund-Consumer: Error occurred while updating balance in fund sub account. Reason: %s", err.Error())
					} else {
						count, err := res.RowsAffected()
						if err != nil {
							fmt.Printf("Fund-Consumer: Error occurred while adjusting fund account %s balance. Reason: %s", from, err.Error())
						} else {
							if count > 0 {
								fmt.Printf("Fund-Consumer: Fund account balance adjusted, id: %s", from)
							} else {
								fmt.Printf("Fund-Consumer: Fund account %s balance adjusted failed to update", from)
							}
						}
					}
				} else {
					fmt.Printf("Fund-Consumer: From account %s balance adjusted failed to update", from)
				}
			}
		}
	}
}

func validate(sub_account_id string, amt float64, db *sql.DB) string {
	var msg string
	var balance float64
	var status string

	sqlStatement := `SELECT balance, status FROM sub_accounts WHERE sub_account_id = $1`
	row := db.QueryRow(sqlStatement, sub_account_id)
	switch err := row.Scan(&balance, &status); err {
	case sql.ErrNoRows:
		msg = fmt.Sprintf("INVALID SUB ACCOUNT ID: %s", sub_account_id)
	case nil:
		// record found with given account number
		// validate account status
		if status == "inactive" {
			msg = fmt.Sprintf("INACTIVE SUB ACCOUNT ID: %s", sub_account_id)
		} else {
			// validate from sub account balance is greater than given amt
			if balance < amt {
				msg = fmt.Sprintf("INSUFFICIENT BALANCE IN SUB ACCOUNT ID: %s", sub_account_id)
			}
		}
	default:
		msg = fmt.Sprintf("DATABASE ERROR: %s", err.Error())
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
