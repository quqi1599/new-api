/*
Copyright (C) 2025 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

import React, { useEffect, useMemo, useState } from 'react';
import {
  Banner,
  Button,
  Empty,
  Input,
  Modal,
  SideSheet,
  Space,
  Tag,
  Typography,
} from '@douyinfe/semi-ui';
import { IconSearch } from '@douyinfe/semi-icons';
import {
  IllustrationNoResult,
  IllustrationNoResultDark,
} from '@douyinfe/semi-illustrations';
import {
  API,
  isRoot,
  showError,
  showSuccess,
  timestamp2string,
} from '../../../../helpers';
import CardTable from '../../../common/ui/CardTable';

const { Text, Title } = Typography;

const ProtectedChannelBansSideSheet = ({ visible, onCancel, t }) => {
  const canUnban = isRoot();
  const [loading, setLoading] = useState(false);
  const [unbanningTokenId, setUnbanningTokenId] = useState(null);
  const [items, setItems] = useState([]);
  const [total, setTotal] = useState(0);
  const [currentPage, setCurrentPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [keyword, setKeyword] = useState('');
  const [appliedKeyword, setAppliedKeyword] = useState('');

  const loadBans = async (
    page = currentPage,
    size = pageSize,
    query = appliedKeyword,
  ) => {
    setLoading(true);
    try {
      const res = await API.get('/api/token/admin/protected_channel_bans', {
        params: { p: page, page_size: size, keyword: query || undefined },
      });
      if (!res.data?.success) {
        showError(res.data?.message || t('加载失败'));
        return;
      }
      const data = res.data.data || {};
      setItems(data.items || []);
      setTotal(data.total || 0);
      setCurrentPage(data.page || page);
    } catch (error) {
      showError(t('请求失败'));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (!visible) return;
    setKeyword('');
    setAppliedKeyword('');
    setCurrentPage(1);
    loadBans(1, pageSize, '');
  }, [visible]);

  const search = () => {
    const query = keyword.trim();
    setAppliedKeyword(query);
    setCurrentPage(1);
    loadBans(1, pageSize, query);
  };

  const unban = (record) => {
    Modal.confirm({
      title: t('解除 API Key 保护限制'),
      content: t(
        '解除后，该 API Key 将可以再次进入所有受保护渠道。确定继续吗？',
      ),
      centered: true,
      onOk: async () => {
        setUnbanningTokenId(record.token_id);
        try {
          const res = await API.delete(
            `/api/token/admin/protected_channel_bans/${record.token_id}`,
          );
          if (!res.data?.success) {
            showError(res.data?.message || t('操作失败'));
            return;
          }
          showSuccess(t('已解除保护限制'));
          const targetPage =
            items.length === 1 && currentPage > 1
              ? currentPage - 1
              : currentPage;
          await loadBans(targetPage, pageSize, appliedKeyword);
        } catch (error) {
          showError(t('请求失败'));
        } finally {
          setUnbanningTokenId(null);
        }
      },
    });
  };

  const columns = useMemo(
    () => [
      {
        title: t('API Key'),
        key: 'token',
        width: 230,
        render: (_, record) => (
          <div className='min-w-0'>
            <div className='font-medium truncate'>
              {record.token_name || t('已删除的令牌')}
            </div>
            <Text type='tertiary' size='small'>
              {t('ID')}: {record.token_id} · {record.masked_key || '-'}
            </Text>
          </div>
        ),
      },
      {
        title: t('所属用户'),
        key: 'user',
        width: 150,
        render: (_, record) => (
          <div>
            <div>{record.username || '-'}</div>
            <Text type='tertiary' size='small'>
              {t('ID')}: {record.user_id || '-'}
            </Text>
          </div>
        ),
      },
      {
        title: t('触发渠道'),
        key: 'channel',
        width: 170,
        render: (_, record) => (
          <div>
            <div>{record.trigger_channel_name || t('已删除的渠道')}</div>
            <Text type='tertiary' size='small'>
              {t('ID')}: {record.trigger_channel_id}
            </Text>
          </div>
        ),
      },
      {
        title: t('原因'),
        key: 'reason',
        width: 150,
        render: () => (
          <Tag color='orange' type='light' shape='circle'>
            {t('上游内容策略封禁')}
          </Tag>
        ),
      },
      {
        title: t('Moderation ID'),
        dataIndex: 'moderation_id',
        key: 'moderation_id',
        width: 260,
        render: (value) =>
          value ? (
            <Text
              code
              copyable={{ content: value }}
              ellipsis={{ showTooltip: true }}
              style={{
                display: 'inline-block',
                maxWidth: 220,
                verticalAlign: 'middle',
              }}
            >
              {value}
            </Text>
          ) : (
            <Text type='tertiary'>-</Text>
          ),
      },
      {
        title: t('封禁时间'),
        dataIndex: 'created_at',
        key: 'created_at',
        width: 170,
        render: (value) => (value ? timestamp2string(value) : '-'),
      },
      ...(canUnban
        ? [
            {
              title: t('操作'),
              key: 'action',
              width: 100,
              fixed: 'right',
              render: (_, record) => (
                <Button
                  size='small'
                  type='warning'
                  theme='light'
                  loading={unbanningTokenId === record.token_id}
                  onClick={() => unban(record)}
                >
                  {t('解除')}
                </Button>
              ),
            },
          ]
        : []),
    ],
    [
      canUnban,
      currentPage,
      items.length,
      pageSize,
      appliedKeyword,
      t,
      unbanningTokenId,
    ],
  );

  return (
    <SideSheet
      visible={visible}
      placement='right'
      width='min(960px, 100vw)'
      bodyStyle={{ padding: 0 }}
      onCancel={onCancel}
      title={
        <Space wrap>
          <Tag color='blue' shape='circle'>
            {t('全局')}
          </Tag>
          <Title heading={4} className='m-0'>
            {t('API Key 保护名单')}
          </Title>
          <Text type='tertiary'>{t('共 {{count}} 条', { count: total })}</Text>
        </Space>
      }
    >
      <div className='p-4 space-y-4'>
        <Banner
          type='info'
          closeIcon={null}
          description={t(
            '名单以 API Key 为单位，与用户账号无关。名单中的 Key 会跳过当前和未来所有已启用内容策略保护的渠道。',
          )}
        />

        <div className='flex flex-col sm:flex-row gap-2'>
          <Input
            value={keyword}
            prefix={<IconSearch />}
            showClear
            placeholder={t('搜索令牌 ID、名称、用户、触发渠道或 Moderation ID')}
            onChange={setKeyword}
            onEnterPress={search}
          />
          <Button type='primary' theme='solid' onClick={search}>
            {t('查询')}
          </Button>
          <Button
            type='tertiary'
            onClick={() => {
              setKeyword('');
              setAppliedKeyword('');
              setCurrentPage(1);
              loadBans(1, pageSize, '');
            }}
          >
            {t('重置')}
          </Button>
        </div>

        <CardTable
          columns={columns}
          dataSource={items}
          rowKey='token_id'
          loading={loading}
          scroll={{ x: 'max-content' }}
          hidePagination={false}
          pagination={{
            currentPage,
            pageSize,
            total,
            pageSizeOpts: [10, 20, 50, 100],
            showSizeChanger: true,
            onPageChange: (page) => loadBans(page, pageSize, appliedKeyword),
            onPageSizeChange: (size) => {
              setPageSize(size);
              setCurrentPage(1);
              loadBans(1, size, appliedKeyword);
            },
          }}
          empty={
            <Empty
              image={
                <IllustrationNoResult style={{ width: 150, height: 150 }} />
              }
              darkModeImage={
                <IllustrationNoResultDark style={{ width: 150, height: 150 }} />
              }
              description={t('暂无被封禁的 API Key')}
              style={{ padding: 30 }}
            />
          }
          size='middle'
        />
      </div>
    </SideSheet>
  );
};

export default ProtectedChannelBansSideSheet;
