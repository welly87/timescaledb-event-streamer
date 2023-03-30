package configuring

import (
	"crypto/tls"
	"github.com/Shopify/sarama"
	"os"
	"reflect"
	"strings"
)

type SinkType string

const (
	Stdout SinkType = "stdout"
	NATS   SinkType = "nats"
	Kafka  SinkType = "kafka"
)

type NamingStrategyType string

const (
	Debezium NamingStrategyType = "debezium"
)

type NatsAuthorizationType string

const (
	UserInfo    NatsAuthorizationType = "userinfo"
	Credentials NatsAuthorizationType = "credentials"
	Jwt         NatsAuthorizationType = "jwt"
)

type PostgreSQLConfig struct {
	Connection  string `toml:"connection"`
	Password    string `toml:"password"`
	Publication string `toml:"publication"`
}

type SinkConfig struct {
	Type  SinkType    `toml:"type"`
	Nats  NatsConfig  `toml:"nats"`
	Kafka KafkaConfig `toml:"kafka"`
}

type TopicConfig struct {
	NamingStrategy TopicNamingStrategyConfig `toml:"namingstrategy"`
	Prefix         string                    `toml:"prefix"`
}

type TimescaleDBConfig struct {
	Hypertables TimescaleHypertablesConfig `toml:"hypertables"`
	Events      TimescaleEventsConfig      `toml:"events"`
}

type NatsUserInfoConfig struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

type NatsCredentialsConfig struct {
	Certificate string   `toml:"certificate"`
	Seeds       []string `toml:"seeds"`
}

type NatsJWTConfig struct {
	JWT  string `toml:"jwt"`
	Seed string `toml:"seed"`
}

type NatsConfig struct {
	Address       string                `toml:"address"`
	Authorization NatsAuthorizationType `toml:"authorization"`
	UserInfo      NatsUserInfoConfig    `toml:"userinfo"`
	Credentials   NatsCredentialsConfig `toml:"credentials"`
	JWT           NatsJWTConfig         `toml:"jwt"`
}

type KafkaSaslConfig struct {
	Enabled   bool                 `toml:"user"`
	User      string               `toml:"user"`
	Password  string               `toml:"password"`
	Mechanism sarama.SASLMechanism `toml:"mechanism"`
}

type KafkaTLSConfig struct {
	Enabled    bool               `toml:"enabled"`
	SkipVerify bool               `toml:"skipverify"`
	ClientAuth tls.ClientAuthType `toml:"clientauth"`
}

type KafkaConfig struct {
	Brokers    []string        `toml:"brokers"`
	Idempotent bool            `toml:"idempotent"`
	Sasl       KafkaSaslConfig `toml:"sasl"`
	TLS        KafkaTLSConfig  `toml:"tls"`
}

type TopicNamingStrategyConfig struct {
	Type NamingStrategyType `toml:"type"`
}

type TimescaleHypertablesConfig struct {
	Excludes []string `toml:"excludes"`
	Includes []string `toml:"includes"`
}

type TimescaleEventsConfig struct {
	Read          bool `toml:"read"`
	Insert        bool `toml:"insert"`
	Update        bool `toml:"update"`
	Delete        bool `toml:"delete"`
	Truncate      bool `toml:"truncate"`
	Compression   bool `toml:"compression"`
	Decompression bool `toml:"decompression"`
}

type Config struct {
	PostgreSQL  PostgreSQLConfig  `toml:"postgresql"`
	Sink        SinkConfig        `toml:"sink"`
	Topic       TopicConfig       `toml:"topic"`
	TimescaleDB TimescaleDBConfig `toml:"timescaledb"`
}

func GetOrDefault[V any](config *Config, canonicalProperty string, defaultValue V) V {
	if env, ok := findEnvProperty(canonicalProperty, defaultValue); ok {
		return env
	}

	properties := strings.Split(canonicalProperty, ".")

	element := reflect.ValueOf(*config)
	for _, property := range properties {
		if e, ok := findProperty(element, property); ok {
			element = e
		} else {
			return defaultValue
		}
	}

	if !element.IsZero() &&
		!(element.Kind() == reflect.Ptr && element.IsNil()) {
		return element.Convert(reflect.TypeOf(defaultValue)).Interface().(V)
	}
	return defaultValue
}

func findEnvProperty[V any](canonicalProperty string, defaultValue V) (V, bool) {
	t := reflect.TypeOf(defaultValue)

	envVarName := strings.ToUpper(canonicalProperty)
	envVarName = strings.ReplaceAll(envVarName, "_", "__")
	envVarName = strings.ReplaceAll(envVarName, ".", "_")
	if val, ok := os.LookupEnv(envVarName); ok {
		v := reflect.ValueOf(val)
		cv := v.Convert(t)
		if !cv.IsZero() &&
			!(cv.Kind() == reflect.Ptr && cv.IsNil()) {
			return cv.Interface().(V), true
		}
	}
	return reflect.Zero(t).Interface().(V), false
}

func findProperty(element reflect.Value, property string) (reflect.Value, bool) {
	t := element.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}

		if f.Tag.Get("toml") == property {
			return element.Field(i), true
		}
	}
	return reflect.Value{}, false
}