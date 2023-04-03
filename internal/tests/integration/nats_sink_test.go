package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-errors/errors"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nats-io/nats.go"
	"github.com/noctarius/event-stream-prototype/internal/configuring"
	"github.com/noctarius/event-stream-prototype/internal/configuring/sysconfig"
	"github.com/noctarius/event-stream-prototype/internal/logging"
	"github.com/noctarius/event-stream-prototype/internal/supporting"
	"github.com/noctarius/event-stream-prototype/internal/systemcatalog/model"
	inttest "github.com/noctarius/event-stream-prototype/internal/testing"
	"github.com/noctarius/event-stream-prototype/internal/testing/containers"
	"github.com/noctarius/event-stream-prototype/internal/testing/testrunner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"testing"
	"time"
)

var natsLogger = logging.NewLogger("Test_Nats_Sink")

type NatsIntegrationTestSuite struct {
	testrunner.TestRunner
}

func TestNatsIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(NatsIntegrationTestSuite))
}

func (nits *NatsIntegrationTestSuite) Test_Nats_Sink() {
	topicPrefix := supporting.RandomTextString(10)

	var natsUrl string
	var natsContainer testcontainers.Container

	nits.RunTest(
		func(ctx testrunner.Context) error {
			// Collect logs
			natsContainer.FollowOutput(&testrunner.ContainerLogForwarder{Logger: natsLogger})
			natsContainer.StartLogProducer(context.Background())

			conn, err := nats.Connect(natsUrl, nats.DontRandomize(), nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1))
			if err != nil {
				return err
			}

			js, err := conn.JetStream(nats.PublishAsyncMaxPending(256))
			if err != nil {
				return err
			}

			subjectName := fmt.Sprintf(
				"%s.%s.%s", topicPrefix,
				testrunner.GetAttribute[string](ctx, "schemaName"),
				testrunner.GetAttribute[string](ctx, "tableName"),
			)

			streamName := supporting.RandomTextString(10)
			groupName := supporting.RandomTextString(10)

			natsLogger.Println("Creating NATS JetStream stream...")
			_, err = js.AddStream(&nats.StreamConfig{
				Name:     streamName,
				Subjects: []string{subjectName},
			})
			if err != nil {
				return err
			}

			collected := make(chan bool, 1)
			envelopes := make([]inttest.Envelope, 0)
			_, err = js.QueueSubscribe(subjectName, groupName, func(msg *nats.Msg) {
				envelope := inttest.Envelope{}
				if err := json.Unmarshal(msg.Data, &envelope); err != nil {
					msg.Nak()
					nits.T().Error(err)
				}
				natsLogger.Printf("EVENT: %+v", envelope)
				envelopes = append(envelopes, envelope)
				if len(envelopes) >= 10 {
					collected <- true
				}
				msg.Ack()
			}, nats.ManualAck())
			if err != nil {
				return err
			}

			if _, err := ctx.Exec(context.Background(),
				fmt.Sprintf(
					"INSERT INTO \"%s\" SELECT ts, ROW_NUMBER() OVER (ORDER BY ts) AS val FROM GENERATE_SERIES('2023-03-25 00:00:00'::TIMESTAMPTZ, '2023-03-25 00:09:59'::TIMESTAMPTZ, INTERVAL '1 minute') t(ts)",
					testrunner.GetAttribute[string](ctx, "tableName"),
				),
			); err != nil {
				return err
			}

			<-collected

			for i, envelope := range envelopes {
				assert.Equal(nits.T(), i+1, int(envelope.Payload.After["val"].(float64)))
			}
			return nil
		},

		testrunner.WithSetup(func(setupContext testrunner.SetupContext) error {
			sn, tn, err := setupContext.CreateHypertable("ts", time.Hour*24,
				model.NewColumn("ts", pgtype.TimestamptzOID, "timestamptz", false, false, nil),
				model.NewColumn("val", pgtype.Int4OID, "integer", false, false, nil),
			)
			if err != nil {
				return err
			}
			testrunner.Attribute(setupContext, "schemaName", sn)
			testrunner.Attribute(setupContext, "tableName", tn)

			nC, nU, err := containers.SetupNatsContainer()
			if err != nil {
				return errors.Wrap(err, 0)
			}
			natsUrl = nU
			natsContainer = nC

			setupContext.AddSystemConfigConfigurator(func(config *sysconfig.SystemConfig) {
				config.Topic.Prefix = topicPrefix
				config.Sink.Type = configuring.NATS
				config.Sink.Nats = configuring.NatsConfig{
					Address:       natsUrl,
					Authorization: configuring.UserInfo,
					UserInfo: configuring.NatsUserInfoConfig{
						Username: "",
						Password: "",
					},
				}
			})

			return nil
		}),

		testrunner.WithTearDown(func(ctx testrunner.Context) error {
			if natsContainer != nil {
				natsContainer.Terminate(context.Background())
			}
			return nil
		}),
	)
}