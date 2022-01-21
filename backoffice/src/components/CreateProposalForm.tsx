import {observer} from "mobx-react";
import {Button, Col, Form, Input, Row} from "antd";
import {PlusOutlined} from "@ant-design/icons";
import {useChilizStore} from "../stores";

export interface IGenerateThresholdKeyFormProps {
  isLoading?: boolean;
  isFetching?: boolean;
}

const CreateProposalForm = observer((props: IGenerateThresholdKeyFormProps) => {
  const store = useChilizStore()
  return (
    <Form
      layout="vertical"
      onFinish={async (values) => {
        const {deployer, description} = values
        const proposeResult = await store.addDeployerProposal(deployer, description),
          proposeReceipt = await proposeResult.receiptPromise
        console.log(proposeResult.transactionHash)
        console.log(proposeReceipt)
        const {blockNumber} = proposeReceipt
        const [recentProposal] = await store.getProposalCreatedEvents({
          fromBlock: blockNumber,
          toBlock: blockNumber,
        })
        const voteResult = await store.voteForProposal(recentProposal.proposalId),
          voteReceipt = await voteResult.receiptPromise
        console.log(voteResult.transactionHash)
        console.log(voteReceipt)
        const executeResult = await store.executeProposal(recentProposal),
          executeReceipt = await executeResult.receiptPromise
        console.log(executeResult.transactionHash)
        console.log(executeReceipt)
      }}
    >
      <Row gutter={24}>
        <Col span={20} offset={2}>
          <Form.Item
            name="deployer"
            extra={<span>Deployer address to add.</span>}
            label="Deployer"
            rules={[
              {required: true, message: 'Required field'},
            ]}
          >
            <Input type={"text"}/>
          </Form.Item>
        </Col>
        <Col span={20} offset={2}>
          <Form.Item
            name="description"
            extra={<span>Description for this proposal.</span>}
            label="Description"
            rules={[
              {required: true, message: 'Required field'},
            ]}
          >
            <Input.TextArea/>
          </Form.Item>
        </Col>
      </Row>
      <Form.Item wrapperCol={{offset: 11}}>
        <Button type="primary" loading={props.isLoading} disabled={props.isLoading} htmlType="submit"
                icon={<PlusOutlined/>}>
          Propose, Vote & Execute
        </Button>
      </Form.Item>
    </Form>
  )
})

export default CreateProposalForm